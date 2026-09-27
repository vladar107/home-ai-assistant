import type { Api, Model } from "@earendil-works/pi-ai";
import type { ProviderKind } from "./types.ts";

export type GoogleRequestBuilder = (model: Model<Api>, body: any) => { url: string; headers: Record<string, string>; body: any };

export type ProviderConfig = {
    kind: ProviderKind;
    searchTool?: string;
    urlContextTool?: string;
    buildRequest?: GoogleRequestBuilder;
};

const GOOGLE_PROVIDERS: Record<string, ProviderConfig> = {
    "google-generative-ai": {
        kind: "google",
        searchTool: "google_search",
        urlContextTool: "url_context",
        buildRequest: (model, body) => ({
            url: `${model.baseUrl}/models/${model.id}:streamGenerateContent?alt=sse`,
            headers: {
                "Content-Type": "application/json",
                "Accept": "text/event-stream",
            },
            body
        })
    }
};

export function getProviderKind(model: Model<Api>): ProviderKind {
    if (GOOGLE_PROVIDERS[model.provider] || GOOGLE_PROVIDERS[model.api]) return "google";
    if (model.provider === "xai" && model.api === "openai-responses") return "xai";
    if (
        model.api === "openai-responses"
        || model.api === "azure-openai-responses"
        || model.api === "openai-codex-responses"
    ) return "openai";
    if (model.api === "anthropic-messages") return "anthropic";
    return "unsupported";
}

export function getConfig(model: Model<Api>): ProviderConfig {
    const googleConfig = GOOGLE_PROVIDERS[model.provider] || GOOGLE_PROVIDERS[model.api];
    if (googleConfig) return googleConfig;
    const kind = getProviderKind(model);
    return { kind };
}
