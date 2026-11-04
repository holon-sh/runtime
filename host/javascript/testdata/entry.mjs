import { start } from "../adapter.bundle.mjs";
await start(() => import("./server.mjs"));
console.log(JSON.stringify({ event: "ready" }));
