// IP-26 DoD 9: every ```ts block in README.md is compiled and run, so a stale example fails the
// build. Run after `npm run build`.
//
// Against a real service when QUOTACORE_URL, QUOTACORE_API_KEY and QUOTACORE_ADMIN_KEY are set,
// which is how CI runs it; otherwise against the in-process fake from the unit tests. The blocks
// import "@quotacore/sdk", which resolves to this package by its own name.
import { execFile, execFileSync } from "node:child_process";
import { mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const pkg = join(dirname(fileURLToPath(import.meta.url)), "..");
const work = join(pkg, ".readme-examples");
const readme = readFileSync(join(pkg, "README.md"), "utf8");
const blocks = [...readme.matchAll(/^```ts\n([\s\S]*?)^```$/gm)].map((m) => m[1]);
if (blocks.length === 0) {
  console.error("readme-examples: README.md has no ts blocks");
  process.exit(1);
}

rmSync(work, { recursive: true, force: true });
mkdirSync(work);
blocks.forEach((code, i) => writeFileSync(join(work, `example-${i + 1}.ts`), `${code}\nexport {};\n`));
writeFileSync(
  join(work, "tsconfig.json"),
  JSON.stringify({
    extends: "../tsconfig.json",
    compilerOptions: { rootDir: ".", outDir: "out", declaration: false, sourceMap: false },
    include: ["*.ts"],
  }),
);
execFileSync(process.execPath, [join(pkg, "node_modules", "typescript", "bin", "tsc"), "-p", work], { stdio: "inherit" });

let fake;
const env = { ...process.env };
if (!env.QUOTACORE_URL || !env.QUOTACORE_API_KEY || !env.QUOTACORE_ADMIN_KEY) {
  if (env.QUOTACORE_SDK_REQUIRE_LIVE) {
    console.error("readme-examples: QUOTACORE_SDK_REQUIRE_LIVE is set, but QUOTACORE_URL, QUOTACORE_API_KEY or QUOTACORE_ADMIN_KEY is not");
    process.exit(1);
  }
  const { startFake } = await import(join(pkg, "dist", "test", "fake-server.js"));
  const started = await startFake();
  fake = started.fake;
  Object.assign(env, { QUOTACORE_URL: started.url, QUOTACORE_API_KEY: "qc_runtime_example.x", QUOTACORE_ADMIN_KEY: "qc_admin_example.x" });
}

let failed = 0;
try {
  for (let i = 1; i <= blocks.length; i++) {
    try {
      const out = await run(join(work, "out", `example-${i}.js`), env);
      console.log(`readme-examples: example ${i} ok${out ? `: ${out.trim().split("\n")[0]}` : ""}`);
    } catch (e) {
      failed++;
      console.error(`readme-examples: example ${i} failed\n${e.stderr ?? e}`);
    }
  }
} finally {
  await fake?.stop();
}
if (failed) process.exit(1);
console.log(`readme-examples: ${blocks.length} examples ran against ${fake ? "the fake" : env.QUOTACORE_URL}`);

function run(file, env) {
  return new Promise((resolve, reject) =>
    execFile(process.execPath, [file], { env, cwd: pkg, timeout: 60_000 }, (err, stdout, stderr) =>
      err ? reject(Object.assign(err, { stderr })) : resolve(stdout),
    ),
  );
}
