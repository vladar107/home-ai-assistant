import type { ExtensionContext, AgentToolUpdateCallback } from "@earendil-works/pi-coding-agent";
import { Type, type Static } from "typebox";
import { callApiStream, getConfig } from "./api.ts";
import { formatResult, formatUrlContextResult } from "./format.ts";
import { getModel, missingConfigResult, errorResult } from "./utils.ts";

export const UrlContextSchema = Type.Object({
    query: Type.String({ description: "Question or task to perform on the URLs" }),
    urls: Type.Array(Type.String(), { 
        description: "Public URLs to analyze (web pages, documents, images, YouTube videos, etc).",
        minItems: 1,
        maxItems: 20
    }),
});
export type UrlContextInput = Static<typeof UrlContextSchema>;

const YOUTUBE_REGEX = /^(?:https?:\/\/)?(?:www\.)?(?:youtube\.com\/(?:watch\?v=|embed\/)|youtu\.be\/)([a-zA-Z0-9_-]{11})/;

export async function urlContext(
    id: string, 
    params: UrlContextInput, 
    signal: AbortSignal, 
    onUpdate: AgentToolUpdateCallback | undefined, 
    ctx: ExtensionContext
) {
    const model = await getModel(ctx);
    if (!model) return missingConfigResult(ctx);

    const count = params.urls.length;
    onUpdate?.({ content: [{ type: "text", text: `Analyzing ${count} URL${count > 1 ? 's' : ''}...` }], details: {} });

    try {
        const config = getConfig(model);
        if (config.kind !== "google") {
            return formatResult(
                `url_context currently requires a Google Gemini-compatible model. Current model: ${model.id} (${model.provider}/${model.api}).\n\nUse web_search for cross-provider web search, or switch to Gemini for provider-native URL context retrieval.`,
                {
                    error: "unsupported_provider",
                    providerKind: config.kind,
                    model: model.id,
                    supportedProviders: ["google", "google-generative-ai"],
                    grounded: false,
                }
            );
        }
        
        let contents: any[] = [];
        let tools: any[] | undefined = [{ [config.urlContextTool!]: {} }];

        // Special handling for YouTube videos on Gemini
        if (model.api === "google-generative-ai") {
            const youtubeUrls: string[] = [];
            const otherUrls: string[] = [];

            for (const url of params.urls) {
                if (YOUTUBE_REGEX.test(url)) {
                    youtubeUrls.push(url);
                } else {
                    otherUrls.push(url);
                }
            }

            // If we have YouTube URLs, construct file_data parts
            if (youtubeUrls.length > 0) {
                const parts: any[] = [];
                
                for (const url of youtubeUrls) {
                    parts.push({
                        file_data: { file_uri: url, mime_type: "video/mp4" }
                    });
                }

                let prompt = params.query;
                if (otherUrls.length > 0) {
                    prompt += `\n\nURLs:\n${otherUrls.join("\n")}`;
                } else {
                    // If no other URLs, we might not need the tool, but keep it just in case
                    // or maybe the tool is required for grounding even with video?
                    // "google_search_retrieval" tool might confuse if there are no URLs to retrieve.
                    // But if we remove the tool, we might lose grounding capabilities (like search).
                    // Let's keep the tool enabled.
                }

                parts.push({ text: prompt });
                contents = [{ role: "user", parts }];
            } else {
                // No YouTube URLs, standard behavior
                const combinedPrompt = `${params.query}\n\nURLs:\n${params.urls.join("\n")}`;
                contents = [{ role: "user", parts: [{ text: combinedPrompt }] }];
            }
        } else {
            // Not Gemini, standard behavior
            const combinedPrompt = `${params.query}\n\nURLs:\n${params.urls.join("\n")}`;
            contents = [{ role: "user", parts: [{ text: combinedPrompt }] }];
        }

        const result = await callApiStream(ctx, model, {
            contents,
            ...(tools ? { tools } : {})
        }, onUpdate, signal);

        return formatUrlContextResult(result, { modelId: model.id });
    } catch (e: any) {
        return errorResult(e);
    }
}
