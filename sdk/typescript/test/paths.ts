// Paths into the repository, from the compiled test's location (dist/test/).

import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

export const packageRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
export const repoRoot = join(packageRoot, "..", "..");
export const repoFile = (p: string) => join(repoRoot, p);
export const packageFile = (p: string) => join(packageRoot, p);
