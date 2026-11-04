import { build } from "esbuild";
import { readFileSync } from "node:fs";
import { builtinModules } from "node:module";

await build({
  absWorkingDir: import.meta.dirname,
  entryPoints: ["adapter.ts"],
  outfile: "adapter.bundle.mjs",
  bundle: true,
  platform: "node",
  external: ["@modelcontextprotocol/server"],
  alias: Object.fromEntries(builtinModules
    .filter((name) => !name.startsWith("node:"))
    .map((name) => [name, `node:${name}`])),
  format: "esm",
  target: "node22",
  legalComments: "inline",
  banner: {
    js: `/*!\n${[
      "@modelcontextprotocol/node/LICENSE",
      "@hono/node-server/LICENSE",
    ]
      .map((path) => readFileSync(new URL(`node_modules/${path}`, import.meta.url), "utf8"))
      .join("\n")}\n*/`,
  },
});
