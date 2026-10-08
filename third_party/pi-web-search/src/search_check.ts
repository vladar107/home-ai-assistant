const CHECK_TIMEOUT_MS = 5000;
const UNAVAILABLE = "Web search is unavailable right now; the search was not run.";

// Asks the Home Agent application whether this query may be sent out. Anything but an explicit
// "allow" from the application, including a missing endpoint or a late or malformed answer, means
// the search does not run; the returned text is what the model reads instead of results.
export async function searchRefusal(query: string, signal?: AbortSignal): Promise<string | undefined> {
    const url = process.env.HOME_AGENT_SEARCH_CHECK_URL;
    const token = process.env.HOME_AGENT_TOOL_BRIDGE_TOKEN;
    if (!url || !token) return UNAVAILABLE;
    try {
        const timeout = AbortSignal.timeout(CHECK_TIMEOUT_MS);
        const response = await fetch(url, {
            method: "POST",
            headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
            body: JSON.stringify({ query }),
            signal: signal ? AbortSignal.any([signal, timeout]) : timeout,
        });
        if (!response.ok) return UNAVAILABLE;
        const decision = await response.json();
        if (decision?.verdict === "allow") return undefined;
        if ((decision?.verdict === "confirm" || decision?.verdict === "deny") && typeof decision.text === "string" && decision.text) {
            return decision.text;
        }
    } catch {
        return UNAVAILABLE;
    }
    return UNAVAILABLE;
}
