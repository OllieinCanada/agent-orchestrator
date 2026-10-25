import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";

// A deflated ZIP with two entries, including a nested file. The affected
// yauzl release can stop after the first entry on Node 26.
const archive = Buffer.from(
	"UEsDBBQAAAAIAAAAIVzoXC2YDgAAAAwAAAAJAAAAZmlyc3QudHh0S8ssKi5RSM0rKarkAgBQSwMEFAAAAAgAAAAhXMM+IY0PAAAADQAAABQAAAByZXNvdXJjZXMvc2Vjb25kLnR4dMtLLS5JTVFIzSspquQCAFBLAQIUAxQAAAAIAAAAIVzoXC2YDgAAAAwAAAAJAAAAAAAAAAAAAACAAQAAAABmaXJzdC50eHRQSwECFAMUAAAACAAAACFcwz4hjQ8AAAANAAAAFAAAAAAAAAAAAAAAgAE1AAAAcmVzb3VyY2VzL3NlY29uZC50eHRQSwUGAAAAAAIAAgB5AAAAdgAAAAAA",
	"base64",
);

test("Forge's packager extracts every ZIP entry", { timeout: 10_000 }, async (t) => {
	// Resolve through Forge so a different hoisted copy cannot hide a broken
	// extract-zip dependency used by the actual packaging path.
	const require = createRequire(import.meta.url);
	const forgeRequire = createRequire(require.resolve("@electron-forge/core"));
	const packagerRequire = createRequire(forgeRequire.resolve("@electron/packager"));
	const { default: extract } = await import(pathToFileURL(packagerRequire.resolve("extract-zip")).href);
	const directory = await mkdtemp(path.join(tmpdir(), "ao-package-extraction-"));
	t.after(() => rm(directory, { recursive: true, force: true }));
	const zipPath = path.join(directory, "electron.zip");
	await writeFile(zipPath, archive);

	await extract(zipPath, { dir: path.join(directory, "out") });

	assert.equal(await readFile(path.join(directory, "out", "first.txt"), "utf8"), "first entry\n");
	assert.equal(await readFile(path.join(directory, "out", "resources", "second.txt"), "utf8"), "nested entry\n");
});
