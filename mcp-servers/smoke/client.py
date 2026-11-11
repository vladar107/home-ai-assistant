"""Call an MCP server the way a client on home-mcp does: list its tools and run one search."""

import json
import sys
import urllib.request

URL = sys.argv[1]
EXPECTED = {
    "search_listings",
    "get_listing_details",
    "get_seller_profile",
    "list_seller_listings",
    "list_categories",
    "list_category_filters",
    "check_new_listings",
    "analyze_prices",
}


def rpc(method, params=None, request_id=None):
    message = {"jsonrpc": "2.0", "method": method}
    if params is not None:
        message["params"] = params
    if request_id is not None:
        message["id"] = request_id
    request = urllib.request.Request(
        URL,
        data=json.dumps(message).encode(),
        headers={
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
            "MCP-Protocol-Version": "2025-06-18",
        },
    )
    with urllib.request.urlopen(request, timeout=60) as response:
        body = response.read().decode()
        content_type = response.headers.get("Content-Type", "")
    if request_id is None:
        return None
    if content_type.startswith("text/event-stream"):
        for line in body.splitlines():
            if line.startswith("data:"):
                event = json.loads(line[5:])
                if event.get("id") == request_id:
                    body = json.dumps(event)
    answer = json.loads(body)
    if "error" in answer:
        raise SystemExit(f"FAIL {method}: {answer['error']}")
    return answer["result"]


initialized = rpc(
    "initialize",
    {
        "protocolVersion": "2025-06-18",
        "capabilities": {},
        "clientInfo": {"name": "home-mcp-smoke", "version": "1"},
    },
    1,
)
print("server:", initialized["serverInfo"], "protocol:", initialized["protocolVersion"])
rpc("notifications/initialized")

tools = {tool["name"] for tool in rpc("tools/list", {}, 2)["tools"]}
print("tools:", ", ".join(sorted(tools)))
if tools != EXPECTED:
    raise SystemExit(f"FAIL tool list changed: {sorted(tools ^ EXPECTED)}")

result = rpc(
    "tools/call",
    {"name": "search_listings", "arguments": {"query": "fiets", "limit": 3}},
    3,
)
if result.get("isError"):
    raise SystemExit(f"FAIL search: {result['content']}")
text = "".join(part.get("text", "") for part in result["content"])
if not text.strip():
    raise SystemExit("FAIL search returned no text")
print("search:", text[:300].replace("\n", " "))
print("OK client path")
