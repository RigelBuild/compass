import { type Component, For, Show } from "solid-js";
import { useRefLinkRules } from "../context";
import { openExternal } from "../open-external";
import { type RefToken, refUrl, tokenizeRefs } from "../refs/grammar";
import { safeHref } from "../safe-url";

type RefTextProps = {
	text: string;
	/** Set inside a <button>, where an <a> is not allowed. */
	inControl?: boolean;
	/** In the board roving group the ref leaves the tab order, as the PR chip does. */
	inRovingGroup?: boolean;
};

const RefLink: Component<{ token: RefToken } & Omit<RefTextProps, "text">> = (
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
					{/* biome-ignore lint/a11y/useSemanticElements: an <a> cannot nest in the card
					   <button>; a role="link" span is the PR chip compromise (DL-097).
					   biome-ignore lint/a11y/useFocusableInteractive: the lowercase Solid 2 `tabindex`
					   below makes it focusable; the rule does not recognize it. */}
					<span
						class="ref-link"
						role="link"
						tabindex={props.inRovingGroup ? -1 : 0}
						title={safe()}
						onClick={(event) => open(event, safe())}
						onDblClick={swallow}
						onKeyDown={(event) => {
							if (event.key !== "Enter" && event.key !== " ") return;
							event.preventDefault();
							event.stopPropagation();
							openExternal(safe());
						}}
					>
						{props.token.text}
					</span>
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
				<RefLink
					token={token}
					inControl={props.inControl}
					inRovingGroup={props.inRovingGroup}
				/>
			)
		}
	</For>
);
