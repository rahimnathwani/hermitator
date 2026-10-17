// Runs one of pi's OAuth logins without a terminal, choosing the device
// code flow, and reports progress as JSON lines on stdout:
//
//   {"type":"device_code","user_code":"ABCD-1234","verification_uri":"https://..."}
//   {"type":"credential","provider":"openai-codex","credential":{...}}
//   {"type":"error","message":"..."}
//
// It uses pi's own implementation (from the pi-ai package that ships with
// pi), so hermitator never has to keep a copy of a provider's auth flow.
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import path from "node:path";
import { pathToFileURL } from "node:url";

const out = (event) => process.stdout.write(JSON.stringify(event) + "\n");
const fail = (message) => {
  out({ type: "error", message });
  process.exit(1);
};

const root = execFileSync("npm", ["root", "-g"]).toString().trim();
const candidates = [
  "@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-ai/dist/providers/all.js",
  "@earendil-works/pi-ai/dist/providers/all.js",
];
let providers;
for (const candidate of candidates) {
  try {
    providers = (await import(pathToFileURL(path.join(root, candidate)))).builtinProviders();
    break;
  } catch {}
}
if (!providers) fail("could not find @earendil-works/pi-ai under " + root);

const providerId = process.argv[2] ?? "openai-codex";
const provider = providers.find((p) => p.id === providerId);
if (!provider?.auth?.oauth) fail(`pi has no OAuth login for provider ${providerId}`);

const controller = new AbortController();
process.on("SIGTERM", () => controller.abort());

try {
  const credential = await provider.auth.oauth.login(
    {
      signal: controller.signal,
      prompt: async (prompt) => {
        if (prompt.type === "select") {
          const choice = prompt.options.find((o) => o.id === "device_code");
          if (choice) return choice.id;
        }
        throw new Error(`login wants interactive input: ${prompt.message}`);
      },
      notify: (event) => {
        if (event.type === "device_code") {
          out({
            type: "device_code",
            user_code: event.userCode,
            verification_uri: event.verificationUri,
            expires_in: event.expiresInSeconds,
          });
        } else if (event.message) {
          out({ type: "info", message: event.message });
        }
      },
    },
    { getDeviceId: () => process.env.HERMITATOR_DEVICE_ID || randomUUID() },
  );
  out({ type: "credential", provider: providerId, credential });
} catch (err) {
  fail(String(err?.message ?? err));
}
