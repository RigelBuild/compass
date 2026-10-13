// biome-ignore lint/style/noRestrictedImports: classify Connect errors returned by the Runner relay
import { Code, ConnectError } from "@connectrpc/connect";

const UNBOUND_SESSION_MESSAGE =
	"Your session is not bound to an agent account (the Runner may be reconnecting); the target was not checked.";
const RELAY_UNAVAILABLE_MESSAGE =
	"The Runner relay is temporarily unavailable; retry the tool call.";

export async function relayCall<T>(call: () => Promise<T>): Promise<T> {
	try {
		return await call();
	} catch (error: unknown) {
		if (!(error instanceof ConnectError)) throw error;
		switch (error.code) {
			case Code.FailedPrecondition:
				throw new Error(UNBOUND_SESSION_MESSAGE, { cause: error });
			case Code.Unavailable:
				throw new Error(RELAY_UNAVAILABLE_MESSAGE, { cause: error });
			default:
				throw error;
		}
	}
}
