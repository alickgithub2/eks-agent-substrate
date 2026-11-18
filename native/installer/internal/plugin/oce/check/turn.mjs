// Runs one turn exactly as the oce-substrate-bridge plugin does, from inside an
// OCE Agent gateway Pod, with the Agent's OCE-rendered plugin config, its
// Substrate API token and router certificate:
//   kubectl -n <tenant> cp turn.mjs <gateway-pod>:/tmp/turn.mjs -c gateway
//   kubectl -n <tenant> exec <gateway-pod> -c gateway -- node /tmp/turn.mjs <conversation> "<message>"
// The same conversation name reaches the same actor, resumed if suspended.
import { readFileSync } from "node:fs";
const conf = JSON.parse(readFileSync(process.env.OPENCLAW_CONFIG_PATH, "utf8"));
const cfg = conf.plugins.entries["oce-substrate-bridge"].config;
const token = cfg.actorToken === "${OPENCLAW_ACTOR_TOKEN}" ? process.env.OPENCLAW_ACTOR_TOKEN : cfg.actorToken;
const d = "/opt/oce-substrate/substrate/dist/";
const { createSubstrateAcpRuntime } = await import(d + "acp-runtime.js");
const { createActorProvisioner } = await import(d + "actor-provisioner.js");
const { createKubectlAteClient } = await import(d + "kubectl-ate-client.js");
const client = createKubectlAteClient({ binPath: cfg.kubectlAtePath, endpoint: cfg.ateapiAddress, tokenFile: cfg.ateTokenFile });
const runtime = createSubstrateAcpRuntime({ atespace: cfg.atespace, template: cfg.template, actorDomain: cfg.actorDomain, actorToken: token,
  provisioner: createActorProvisioner({ atespace: cfg.atespace, ateapi: client, egressPolicy: cfg.egressPolicy }) });
const sessionKey = "agent:main:oce-substrate-check:" + process.argv[2];
const t0 = Date.now();
const handle = await runtime.ensureSession({ sessionKey, agent: "main", mode: "persistent" });
console.log("actor ready for", sessionKey, "in", Date.now() - t0, "ms");
let reply = "";
for await (const ev of runtime.runTurn({ handle, text: process.argv[3], mode: "prompt", requestId: "check" })) {
  if (ev.type === "text_delta") reply += ev.text; else if (ev.type === "error") console.log("error event:", ev.message);
}
console.log("REPLY:", JSON.stringify(reply), "total", Date.now() - t0, "ms");
