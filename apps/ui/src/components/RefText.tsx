import { type Component, For, Show } from "solid-js";
import { useRefLinkRules } from "../context";
import { openExternal } from "../open-external";
import { type RefToken, refUrl, tokenizeRefs } from "../refs/grammar";
import { safeHref } from "../safe-url";

type RefTextProps = {
	text: string;
	/** Set inside a <button>, where an <a> is not allowed. */
	inControl?: boolean;
};

const RefLink: Component<{ token: RefToken; inControl?: boolean }> = (
	props,
) => {
	const refLinkRules = useRefLinkRules();
	const href = () => safeHref(refUrl(props.token, refLinkRules()));
	// The ref acts alone: the enclosing card must not also select or open.
	const open = (event: MouseEvent, safe: string) => {
		event.preventDefault();
		event.stopPropagation();
		openExternal(safe);
	};
	const swallow = (event: MouseEvent) => event.stopPropagation();
	return (
		<Show when={href()} fallback={props.token.text}>
			{(safe) => (
				<Show
					when={props.inControl}
					fallback={
						<a
							href={safe()}
							target="_blank"
							rel="noreferrer noopener"
							onClick={(event) => open(event, safe())}
							onDblClick={swallow}
						>
							{props.token.text}
						</a>
					}
				>
					{/* biome-ignore-start lint/a11y/useSemanticElements lint/a11y/useFocusableInteractive lint/a11y/useKeyWithClickEvents: an <a>
					   cannot nest in the card <button>, which is the tab stop (as the PR chip). */}
					<span
						class="ref-link"
						role="link"
						title={safe()}
						onClick={(event) => open(event, safe())}
						onDblClick={swallow}
					>
						{props.token.text}
					</span>
					{/* biome-ignore-end lint/a11y/useSemanticElements lint/a11y/useFocusableInteractive lint/a11y/useKeyWithClickEvents: see above */}
				</Show>
			)}
		</Show>
	);
};

export const RefText: Component<RefTextProps> = (props) => (
	<For each={tokenizeRefs(props.text)}>
		{(token) =>
			typeof token === "string" ? (
				token
			) : (
				<RefLink token={token} inControl={props.inControl} />
			)
		}
	</For>
);
