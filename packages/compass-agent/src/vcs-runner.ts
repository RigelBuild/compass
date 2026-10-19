export interface CommandResult {
	stdout: string;
	stderr: string;
	code: number;
	timedOut?: boolean;
	// Set instead of stdout when the call asked for binary output.
	bytes?: Uint8Array;
}

export interface CommandOptions {
	timeoutMs?: number;
	binary?: boolean;
}

export interface CommandRunner {
	run(
		args: readonly string[],
		cwd: string,
		options?: CommandOptions,
	): Promise<CommandResult>;
}

export async function runCommand(
	command: string,
	args: readonly string[],
	cwd: string,
	options: CommandOptions = {},
): Promise<CommandResult> {
	const timed = options.timeoutMs !== undefined;
	const proc = Bun.spawn([command, ...args], {
		cwd,
		stdin: "ignore",
		stdout: "pipe",
		stderr: "pipe",
		// Its own group, so a timeout also stops the children a hook starts.
		detached: timed,
	});
	const done: Promise<CommandResult> = Promise.all([
		proc.exited,
		new Response(proc.stdout).bytes(),
		new Response(proc.stderr).text(),
	]).then(([code, out, stderr]) =>
		options.binary
			? { code, stdout: "", stderr, bytes: out }
			: { code, stdout: new TextDecoder().decode(out), stderr },
	);
	if (options.timeoutMs === undefined) return done;
	let timer: Timer | undefined;
	const expired = new Promise<undefined>((resolve) => {
		// biome-ignore lint/style/noRestrictedGlobals: the OQ-9 wall-clock limit on a child process is a real deadline, not a test sleep.
		timer = setTimeout(() => resolve(undefined), options.timeoutMs);
	});
	try {
		const result = await Promise.race([done, expired]);
		if (result) return result;
		// The negative pid names the group this call created; nothing else is in it.
		try {
			process.kill(-proc.pid, "SIGKILL");
		} catch (error) {
			// The group can exit between the timeout and the kill; that is still a timeout.
			if ((error as NodeJS.ErrnoException).code !== "ESRCH") throw error;
		}
		// Children can hold the pipes open past the kill, so the output is not awaited.
		return { stdout: "", stderr: "", code: -1, timedOut: true };
	} finally {
		clearTimeout(timer);
	}
}

export const jjRunner: CommandRunner = {
	run: (args, cwd, options) => runCommand("jj", args, cwd, options),
};

export const gitRunner: CommandRunner = {
	run: (args, cwd, options) => runCommand("git", args, cwd, options),
};
