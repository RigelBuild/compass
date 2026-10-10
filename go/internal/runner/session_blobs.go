//go:build unix

package runner

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

const imageUnavailableText = "[image unavailable after resume]"

const blobRefPrefix = "blob:sha256:"

func blobRefsNewestFirst(resumeBody string) []string {
	seen := make(map[string]struct{})
	refs := make([]string, 0, maxResumeBlobCount)
	for offset := len(resumeBody); offset > 0 && len(refs) < maxResumeBlobCount; {
		rel := strings.LastIndex(resumeBody[:offset], blobRefPrefix)
		if rel < 0 {
			break
		}
		start := rel + len(blobRefPrefix)
		end := start + 64
		if end <= len(resumeBody) && isLowerHex(resumeBody[start:end]) {
			sha := resumeBody[start:end]
			if _, exists := seen[sha]; !exists {
				seen[sha] = struct{}{}
				refs = append(refs, sha)
			}
		}
		offset = rel
	}
	return refs
}

func isLowerHex(value string) bool {
	for i := range len(value) {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func markAbsentBlobs(resumeBody string, absent map[string]struct{}) (string, int) {
	if len(absent) == 0 || !strings.Contains(resumeBody, blobRefPrefix) {
		return resumeBody, 0
	}
	var result strings.Builder
	result.Grow(len(resumeBody))
	replacements := 0
	for len(resumeBody) > 0 {
		newline := strings.IndexByte(resumeBody, '\n')
		line, ending := resumeBody, ""
		if newline >= 0 {
			line, ending = resumeBody[:newline], resumeBody[newline:newline+1]
			resumeBody = resumeBody[newline+1:]
		} else {
			resumeBody = ""
		}
		if strings.HasSuffix(line, "\r") {
			line = line[:len(line)-1]
			ending = "\r" + ending
		}
		if !strings.Contains(line, blobRefPrefix) {
			result.WriteString(line)
			result.WriteString(ending)
			continue
		}
		var value any
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			result.WriteString(line)
			result.WriteString(ending)
			continue
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			result.WriteString(line)
			result.WriteString(ending)
			continue
		}
		lineReplacements := markImageValue(value, absent)
		if lineReplacements == 0 {
			result.WriteString(line)
			result.WriteString(ending)
			continue
		}
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(value); err != nil {
			result.WriteString(line)
			result.WriteString(ending)
			continue
		}
		result.Write(bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'}))
		result.WriteString(ending)
		replacements += lineReplacements
	}
	if replacements == 0 {
		return result.String(), 0
	}
	return result.String(), replacements
}

func markImageValue(value any, absent map[string]struct{}) int {
	switch node := value.(type) {
	case map[string]any:
		replacements := 0
		if content, ok := node["content"].([]any); ok {
			for i, blockValue := range content {
				block, ok := blockValue.(map[string]any)
				if !ok || block["type"] != "image" {
					continue
				}
				data, ok := block["data"].(string)
				if !ok || !strings.HasPrefix(data, blobRefPrefix) {
					continue
				}
				sha := strings.TrimPrefix(data, blobRefPrefix)
				if len(sha) != 64 || !isLowerHex(sha) {
					continue
				}
				if _, missing := absent[sha]; missing {
					content[i] = map[string]any{"type": "text", "text": imageUnavailableText}
					replacements++
				}
			}
		}
		for key, item := range node {
			if key != "content" {
				replacements += markImageValue(item, absent)
			}
		}
		return replacements
	case []any:
		replacements := 0
		for _, item := range node {
			replacements += markImageValue(item, absent)
		}
		return replacements
	default:
		return 0
	}
}
