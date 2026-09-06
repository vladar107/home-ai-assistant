// Tool bridge extension written by the Home Agent application for one conversation turn.
// It registers exactly the tools listed in the turn's tools.json and forwards every call to the
// application over the loopback address and bearer token found in the process environment.
import { readFileSync } from "node:fs";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

interface BridgeTool {
	name: string;
	description: string;
	parameters: Record<string, unknown>;
}

interface BridgeResult {
	text: string;
	isError?: boolean;
}

export default function (pi: ExtensionAPI) {
	const url = process.env.HOME_AGENT_TOOL_BRIDGE_URL;
	const token = process.env.HOME_AGENT_TOOL_BRIDGE_TOKEN;
	const toolsPath = process.env.HOME_AGENT_TOOL_BRIDGE_TOOLS;
	if (!url || !token || !toolsPath) {
		throw new Error("tool bridge environment is incomplete");
	}
	const tools = JSON.parse(readFileSync(toolsPath, "utf8")) as BridgeTool[];
	for (const tool of tools) {
		pi.registerTool({
			name: tool.name,
			label: tool.name,
			description: tool.description,
			// Pi validates plain JSON Schema objects before execute runs.
			parameters: tool.parameters as any,
			async execute(_toolCallId, params, signal) {
				const response = await fetch(url, {
					method: "POST",
					headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
					body: JSON.stringify({ name: tool.name, arguments: params ?? {} }),
					signal,
				});
				if (!response.ok) {
					throw new Error(`tool bridge rejected the call (HTTP ${response.status})`);
				}
				const result = (await response.json()) as BridgeResult;
				if (result.isError) {
					throw new Error(result.text);
				}
				return { content: [{ type: "text", text: result.text }], details: {} };
			},
		});
	}
}
