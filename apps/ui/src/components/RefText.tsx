import { type Component, For, Show } from "solid-js";
import { useRefLinkRules } from "../context";
import { openExternal } from "../open-external";
import { type RefToken, refUrl, tokenizeRefs } from "../refs/grammar";
import { safeHref } from "../safe-url";

const RefLink: Component<{ token: RefToken }> = (props) => {
	const refLinkRules = useRefLinkRules();
	const href = () => safeHref(refUrl(props.token, refLinkRules()));
	return (
		<Show when={href()} fallback={props.token.text}>
			{(safe) => (
				<a
					href={safe()}
					target="_blank"
					rel="noreferrer noopener"
					onClick={(event) => {
						event.preventDefault();
						event.stopPropagation();
						openExternal(safe());
					}}
				>
					{props.token.text}
				</a>
			)}
		</Show>
	);
};

export const RefText: Component<{ text: string }> = (props) => (
	<For each={tokenizeRefs(props.text)}>
		{(token) => (typeof token === "string" ? token : <RefLink token={token} />)}
	</For>
);
