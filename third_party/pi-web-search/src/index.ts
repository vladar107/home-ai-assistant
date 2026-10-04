import type { AgentToolResult, ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Text } from "@earendil-works/pi-tui";
import { webSearch, WebSearchSchema } from "./web_search.ts";

const WEB_SEARCH_TOOL = "web_search";

export default function (pi: ExtensionAPI) {
    pi.registerTool({
        name: WEB_SEARCH_TOOL,
        label: "Web Search",
        description: "Search the web using the current supported provider (Google Gemini, xAI Grok, OpenAI, Anthropic, or OpenCode Zen/Go).",
        parameters: WebSearchSchema,
        execute: (id, params, signal = new AbortController().signal, onUpdate, ctx): Promise<AgentToolResult<any>> =>
            webSearch(id, params, signal, onUpdate, ctx, pi.getThinkingLevel()),
        renderCall(args, theme) {
            const query = args.query || "…";
            return new Text(
                `${theme.fg("toolTitle", theme.bold("web_search"))} ${theme.fg("accent", query)}`,
                0,
                0,
            );
        },
        renderResult(result, { expanded }, theme) {
            const output = result.content
                .filter((part) => part.type === "text")
                .map((part) => part.text)
                .join("\n");

            const isError = Boolean(result.details?.error);
            if (!expanded && !isError) return new Text("", 0, 0);

            return new Text(theme.fg(isError ? "error" : "toolOutput", output), 0, 0);
        }
    });
}
