import type { ExtensionContext, AgentToolUpdateCallback } from "@earendil-works/pi-coding-agent";
import type { ModelThinkingLevel } from "@earendil-works/pi-ai";
import { Type, type Static } from "typebox";
import { callApiStream, getConfig } from "./api.ts";
import { formatWebSearchResult } from "./format.ts";
import { searchRefusal } from "./search_check.ts";
import { getWebSearchModel, missingWebSearchConfigResult, errorResult } from "./utils.ts";

export const WebSearchSchema = Type.Object({
    query: Type.String({ description: "The search query or question to answer" }),
});
export type WebSearchInput = Static<typeof WebSearchSchema>;

export async function webSearch(
    id: string, 
    params: WebSearchInput, 
    signal: AbortSignal,
    onUpdate: AgentToolUpdateCallback | undefined, 
    ctx: ExtensionContext,
    thinkingLevel?: ModelThinkingLevel
) {
    const refusal = await searchRefusal(params.query, signal);
    if (refusal) return { content: [{ type: "text", text: refusal }], details: {} };

    const model = await getWebSearchModel(ctx);
    if (!model) return missingWebSearchConfigResult(ctx);

    onUpdate?.({
        content: [{ type: "text", text: `Searching for "${params.query}"...` }],
        details: {}
    });

    try {
        const config = getConfig(model);
        
        // Enable provider-native search tools. Google needs explicit Gemini tool names;
        // OpenAI/Anthropic are handled inside callApiStream based on the current model.
        const tools = config.kind === "google"
            ? [{ [config.searchTool!]: {} }]
            : undefined;

        const result = await callApiStream(ctx, model, {
            contents: [{ role: "user", parts: [{ text: params.query }] }],
            ...(tools ? { tools } : {})
        }, onUpdate, signal, thinkingLevel);

        return formatWebSearchResult(result, { modelId: model.id });
    } catch (e: any) {
        return errorResult(e);
    }
}
