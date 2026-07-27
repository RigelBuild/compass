import { diffArrays } from "diff";

export interface DiffRow {
	kind: "add" | "del";
	text: string;
}

/** Split newline-terminated text into content lines. A single terminal newline
 *  does NOT yield a phantom empty final line, but interior and EOF blank lines
 *  are preserved. Empty input yields no lines. */
export function splitLines(text: string): string[] {
	if (text === "") return [];
	const lines = text.split("\n");
	if (lines.length > 0 && lines[lines.length - 1] === "") lines.pop();
	return lines;
}

/** Interim edit-distance bound for the line diff. It keeps worst-case render
 *  latency bounded: beyond this many edits, `diffRows` skips the fine-grained
 *  diff and renders a coarse all-dels-then-all-adds fallback instead of
 *  freezing on a pathological input. Tunable as real-world diff sizes settle. */
export const DEFAULT_MAX_EDIT_LENGTH = 2000;

export function diffRows(
	oldText: string | null,
	newText: string,
	maxEditLength: number = DEFAULT_MAX_EDIT_LENGTH,
): DiffRow[] {
	const oldLines = splitLines(oldText ?? "");
	const newLines = splitLines(newText);
	const parts = diffArrays(oldLines, newLines, { maxEditLength });
	if (parts === undefined) {
		const rows: DiffRow[] = [];
		for (const v of oldLines) rows.push({ kind: "del", text: v });
		for (const v of newLines) rows.push({ kind: "add", text: v });
		return rows;
	}
	const rows: DiffRow[] = [];
	for (const part of parts) {
		if (!part.added && !part.removed) continue;
		if (part.removed) {
			for (const v of part.value) rows.push({ kind: "del", text: v });
		} else {
			for (const v of part.value) rows.push({ kind: "add", text: v });
		}
	}
	return rows;
}
