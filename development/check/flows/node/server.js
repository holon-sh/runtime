import { existsSync, readFileSync, writeFileSync } from "node:fs";

writeFileSync("/tmp/runtime.json", JSON.stringify({
  installer: existsSync("node_modules/.modules.yaml")
    ? /^packageManager: (.+)$/m.exec(readFileSync("node_modules/.modules.yaml", "utf8"))?.[1] : null,
  marker: "first", version: process.versions.node, uid: process.getuid(),
}));
export const frontend = new URL("file:///tmp/");
export default { fetch: async () => new Response() };
