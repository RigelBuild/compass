import type { RefLinkRule } from "./grammar";
import { refUrl, tokenizeRefs } from "./grammar";

interface MarkdownNode {
	type: string;
	value?: string;
	url?: string;
	title?: string | null;
	children?: unknown[];
}

interface MarkdownParent extends MarkdownNode {
	children: unknown[];
}

export type Plugin = () => (tree: unknown) => void;

function isNode(value: unknown): value is MarkdownNode {
	return (
		typeof value === "object" &&
		value !== null &&
		"type" in value &&
		typeof value.type === "string"
	);
}

function isParent(value: MarkdownNode): value is MarkdownParent {
	return Array.isArray(value.children);
}

function splitText(
	value: string,
	rules: readonly RefLinkRule[],
): MarkdownNode[] {
	const children: MarkdownNode[] = [];
	for (const token of tokenizeRefs(value)) {
		if (typeof token === "string") {
			children.push({ type: "text", value: token });
			continue;
		}
		const url = refUrl(token, rules);
		if (!url) {
			children.push({ type: "text", value: token.text });
			continue;
		}
		children.push({
			type: "link",
			url,
			title: null,
			children: [{ type: "text", value: token.text }],
		});
	}
	return children;
}

function visit(parent: MarkdownParent, rules: readonly RefLinkRule[]): void {
	const children: unknown[] = [];
	for (const value of parent.children) {
		if (!isNode(value)) {
			children.push(value);
			continue;
		}
		if (value.type === "text" && typeof value.value === "string") {
			children.push(...splitText(value.value, rules));
			continue;
		}
		if (
			value.type !== "link" &&
			value.type !== "linkReference" &&
			value.type !== "inlineCode" &&
			value.type !== "code" &&
			isParent(value)
		) {
			visit(value, rules);
		}
		children.push(value);
	}
	parent.children = children;
}

function isParentRoot(value: unknown): value is MarkdownParent {
	return isNode(value) && isParent(value);
}

export function remarkRefLinks(rules: () => readonly RefLinkRule[]): Plugin {
	return () => (tree) => {
		if (isParentRoot(tree)) visit(tree, rules());
	};
}
