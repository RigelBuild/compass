//! Accumulates incomplete UTF-8 sequences across PTY read boundaries.
//!
//! PTY reads use fixed-size buffers (typically 4 KB) that can split multi-byte
//! UTF-8 characters. `String::from_utf8_lossy` would replace both halves with
//! U+FFFD. This accumulator buffers the trailing incomplete bytes and prepends
//! them to the next read, returning `Cow<str>` (borrowed when the input is
//! already valid UTF-8).

use std::borrow::Cow;

/// Maximum number of bytes in an incomplete UTF-8 sequence (a code point is at
/// most 4 bytes).
const MAX_PARTIAL: usize = 4;

/// Buffers incomplete trailing UTF-8 bytes across successive `accumulate` calls.
pub struct Utf8Accumulator {
    /// Leftover bytes from the previous call (0..=3 bytes).
    partial: [u8; MAX_PARTIAL],
    partial_len: usize,
}

impl Utf8Accumulator {
    pub fn new() -> Self {
        Self {
            partial: [0; MAX_PARTIAL],
            partial_len: 0,
        }
    }

    /// Accumulate raw bytes and return a valid UTF-8 string.
    ///
    /// Any incomplete trailing sequence is buffered internally and prepended to
    /// the next call. Invalid bytes that cannot form a valid sequence are
    /// replaced with U+FFFD.
    pub fn accumulate<'a>(&mut self, data: &'a [u8]) -> Cow<'a, str> {
        // Fast path: no leftover and input is valid UTF-8.
        if self.partial_len == 0
            && let Ok(s) = std::str::from_utf8(data)
        {
            return Cow::Borrowed(s);
        }

        // Slow path: combine leftover + new data, split at valid boundary.
        let mut buf: Vec<u8>;
        let combined: &[u8] = if self.partial_len > 0 {
            buf = Vec::with_capacity(self.partial_len + data.len());
            buf.extend_from_slice(&self.partial[..self.partial_len]);
            buf.extend_from_slice(data);
            self.partial_len = 0;
            &buf
        } else {
            data
        };

        // Find the boundary between complete and trailing incomplete UTF-8.
        let valid_up_to = utf8_valid_prefix_len(combined);
        let trailing = &combined[valid_up_to..];

        if trailing.is_empty() {
            // Everything is valid UTF-8.
            let s = unsafe { std::str::from_utf8_unchecked(combined) };
            return Cow::Owned(s.to_owned());
        }

        if is_incomplete_start(trailing) && trailing.len() < MAX_PARTIAL {
            // Buffer the trailing incomplete sequence for next call.
            self.partial[..trailing.len()].copy_from_slice(trailing);
            self.partial_len = trailing.len();
            let s = String::from_utf8_lossy(&combined[..valid_up_to]);
            return Cow::Owned(s.into_owned());
        }

        // Trailing bytes aren't a valid start — let lossy handle everything.
        Cow::Owned(String::from_utf8_lossy(combined).into_owned())
    }
}

/// Returns the byte length of the longest valid UTF-8 prefix.
fn utf8_valid_prefix_len(data: &[u8]) -> usize {
    match std::str::from_utf8(data) {
        Ok(_) => data.len(),
        Err(e) => e.valid_up_to(),
    }
}

/// Check if `bytes` looks like the start of a valid multi-byte UTF-8 sequence
/// that is simply incomplete (truncated).
fn is_incomplete_start(bytes: &[u8]) -> bool {
    if bytes.is_empty() {
        return false;
    }
    let lead = bytes[0];
    let expected_len = match lead {
        0xC0..=0xDF => 2,
        0xE0..=0xEF => 3,
        0xF0..=0xF7 => 4,
        _ => return false,
    };
    if bytes.len() >= expected_len {
        // We have enough bytes but they weren't valid — not just incomplete.
        return false;
    }
    // Verify continuation bytes are valid (0x80..=0xBF).
    bytes[1..].iter().all(|&b| (0x80..=0xBF).contains(&b))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ascii_fast_path_borrows() {
        let mut acc = Utf8Accumulator::new();
        let result = acc.accumulate(b"hello world");
        assert!(matches!(result, Cow::Borrowed(_)));
        assert_eq!(&*result, "hello world");
    }

    #[test]
    fn valid_utf8_fast_path_borrows() {
        let mut acc = Utf8Accumulator::new();
        let data = "こんにちは".as_bytes();
        let result = acc.accumulate(data);
        assert!(matches!(result, Cow::Borrowed(_)));
        assert_eq!(&*result, "こんにちは");
    }

    #[test]
    fn split_two_byte_char() {
        let mut acc = Utf8Accumulator::new();
        // 'é' = 0xC3 0xA9
        let result1 = acc.accumulate(&[0xC3]);
        assert_eq!(&*result1, "");
        let result2 = acc.accumulate(&[0xA9]);
        assert_eq!(&*result2, "é");
    }

    #[test]
    fn split_three_byte_char() {
        let mut acc = Utf8Accumulator::new();
        // '€' = 0xE2 0x82 0xAC
        let result1 = acc.accumulate(b"abc\xE2\x82");
        assert_eq!(&*result1, "abc");
        let result2 = acc.accumulate(b"\xACdef");
        assert_eq!(&*result2, "€def");
    }

    #[test]
    fn split_four_byte_char() {
        let mut acc = Utf8Accumulator::new();
        // '𝕳' = 0xF0 0x9D 0x95 0xB3
        let result1 = acc.accumulate(b"x\xF0\x9D\x95");
        assert_eq!(&*result1, "x");
        let result2 = acc.accumulate(b"\xB3y");
        assert_eq!(&*result2, "𝕳y");
    }

    #[test]
    fn split_four_byte_at_one() {
        let mut acc = Utf8Accumulator::new();
        let result1 = acc.accumulate(b"\xF0");
        assert_eq!(&*result1, "");
        let result2 = acc.accumulate(b"\x9D\x95\xB3");
        assert_eq!(&*result2, "𝕳");
    }

    #[test]
    fn invalid_byte_replaced() {
        let mut acc = Utf8Accumulator::new();
        let result = acc.accumulate(b"hello\xFFworld");
        assert_eq!(&*result, "hello\u{FFFD}world");
    }

    #[test]
    fn empty_input() {
        let mut acc = Utf8Accumulator::new();
        let result = acc.accumulate(b"");
        assert_eq!(&*result, "");
    }

    #[test]
    fn consecutive_splits() {
        let mut acc = Utf8Accumulator::new();
        // Two consecutive 3-byte chars split across boundaries:
        // '日' = 0xE6 0x97 0xA5, '本' = 0xE6 0x9C 0xAC
        let r1 = acc.accumulate(b"\xE6\x97");
        assert_eq!(&*r1, "");
        let r2 = acc.accumulate(b"\xA5\xE6\x9C");
        assert_eq!(&*r2, "日");
        let r3 = acc.accumulate(b"\xAC");
        assert_eq!(&*r3, "本");
    }
}
