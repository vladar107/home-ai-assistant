import type { ExtensionContext, AgentToolUpdateCallback } from "@earendil-works/pi-coding-agent";
import type { Api, Model, ModelThinkingLevel } from "@earendil-works/pi-ai";
import { getProviderKind } from "./providers/config.ts";
import { callGoogleStream, extractPromptFromGeminiBody } from "./providers/google.ts";
import { callOpenAIStream } from "./providers/openai.ts";
import { callAnthropicStream } from "./providers/anthropic.ts";
import type { StreamResult } from "./providers/types.ts";

export { getProviderKind, getConfig } from "./providers/config.ts";
export { applyCitations } from "./providers/google.ts";
export type { Source, SearchResultDetail, NativeSearchCallDetail, StreamResult } from "./providers/types.ts";

export async function callApiStream(
    ctx: ExtensionContext,
    model: Model<Api>,
    body: any,
    onUpdate?: AgentToolUpdateCallback,
    signal?: AbortSignal,
    thinkingLevel?: ModelThinkingLevel
): Promise<StreamResult> {
    const kind = getProviderKind(model);
    if (kind === "google") {
        return callGoogleStream(ctx, model, body, onUpdate, signal);
    }

    const prompt = extractPromptFromGeminiBody(body);
    if (!prompt) {
        throw new Error("No prompt text found in request body");
    }

    if (kind === "openai" || kind === "xai") {
        return callOpenAIStream(ctx, model, prompt, onUpdate, signal, thinkingLevel);
    }
    if (kind === "anthropic") {
        return callAnthropicStream(ctx, model, prompt, onUpdate, signal);
    }

    throw new Error(`Unsupported provider for web search: ${model.provider} (${model.api})`);
}
