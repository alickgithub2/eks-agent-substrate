// Turns through an OCE Agent gateway's own HTTP chat API, run inside the
// gateway Pod (loopback, so OCE's Driver-managed gateway password, which
// never leaves the Pod, authenticates it): the conversation's turns go to
// its Substrate actor through the oce-substrate-bridge plugin.
//   node --input-type=module -e "$(cat chat.mjs)" <conversation> <message>...
// Each message prints "HTTP <status> <ms> ms: <reply as JSON>".
const [conversation, ...messages] = process.argv.slice(1);
for (const content of messages) {
  const t0 = Date.now();
  const r = await fetch("http://127.0.0.1:" + process.env.OPENCLAW_GATEWAY_PORT + "/v1/chat/completions", {
    method: "POST",
    headers: { "content-type": "application/json", authorization: "Bearer " + process.env.OPENCLAW_GATEWAY_PASSWORD },
    body: JSON.stringify({ model: "openclaw/default", stream: false, user: conversation, messages: [{ role: "user", content }] }),
  });
  let j;
  try { j = await r.json(); } catch { j = {}; }
  console.log("HTTP " + r.status + " " + (Date.now() - t0) + " ms: " + JSON.stringify(j.choices?.[0]?.message?.content ?? j.error ?? j).slice(0, 400));
}
