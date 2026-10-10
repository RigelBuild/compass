// Inline styles for the pre-render boot screens (the native connect screen and the
// browser token screen). They run before any stylesheet is guaranteed loaded, so they
// cannot use app.css classes.

export const SCREEN_STYLE = [
	"margin:0",
	"padding:2rem",
	"font:14px/1.6 ui-monospace,SFMono-Regular,Menlo,monospace",
	"color:#e6e6e6",
	"background:#1a1a1a",
	"min-height:100vh",
].join(";");
export const HEADING_STYLE = "margin:0 0 1rem;font-size:1rem";
export const URL_STYLE = ["margin:0 0 1.5rem", "color:#9c9c9c"].join(";");
export const DETAIL_STYLE = [
	"margin:0 0 1rem",
	"white-space:pre-wrap",
	"color:#ff9c9c",
].join(";");
export const INPUT_STYLE = [
	"width:100%",
	"box-sizing:border-box",
	"padding:0.5rem",
	"font:inherit",
	"color:#e6e6e6",
	"background:#111",
	"border:1px solid #444",
].join(";");
export const BUTTON_STYLE = [
	"margin-top:1rem",
	"padding:0.5rem 1rem",
	"font:inherit",
	"cursor:pointer",
].join(";");
