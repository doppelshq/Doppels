import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import { resolve } from "node:path";

export const BUNDLE_DOMAIN = Buffer.from("doppels.so/schema-bundle-sha256/v1\0", "utf8");

export function schemaResource(source) {
  const bytes = Buffer.isBuffer(source) ? source : Buffer.from(source);
  const document = JSON.parse(bytes.toString("utf8"));
  if (!document || typeof document !== "object" || Array.isArray(document)) {
    throw new Error("schema resource must be an object");
  }
  if (typeof document.$id !== "string" || !document.$id) {
    throw new Error("schema resource must declare a non-empty $id");
  }
  const id = new URL(document.$id);
  if (id.hash || id.href !== document.$id) {
    throw new Error(`schema resource has fragmented or non-canonical $id ${document.$id}`);
  }
  return { id: document.$id, source: bytes, document };
}

export async function loadSchemaResources(directory, filenames) {
  const resources = new Map();
  for (const filename of [...filenames].sort()) {
    const resource = schemaResource(await readFile(resolve(directory, filename)));
    if (resources.has(resource.id)) throw new Error(`duplicate schema $id ${resource.id}`);
    resources.set(resource.id, resource);
  }
  return resources;
}

export function schemaBundle(rootId, resources) {
  const state = new Map();
  const bundle = [];

  function visit(id) {
    const resource = resources.get(id);
    if (!resource) throw new Error(`schema bundle references unfixed external resource ${id}`);
    if (state.get(id) === "visiting") {
      throw new Error(`schema bundle contains an external $ref cycle at ${id}`);
    }
    if (state.get(id) === "visited") return;
    state.set(id, "visiting");
    for (const reference of externalReferences(resource)) visit(reference);
    state.set(id, "visited");
    bundle.push(resource);
  }

  visit(rootId);
  return bundle.sort((left, right) => Buffer.compare(Buffer.from(left.id), Buffer.from(right.id)));
}

function externalReferences(resource) {
  const references = new Set();

  function walk(value, base) {
    if (Array.isArray(value)) {
      for (const item of value) walk(item, base);
      return;
    }
    if (!value || typeof value !== "object") return;

    let nextBase = base;
    if (Object.hasOwn(value, "$id")) {
      if (typeof value.$id !== "string" || !value.$id) {
        throw new Error(`schema ${resource.id} contains a non-string or empty $id`);
      }
      nextBase = new URL(value.$id, base).href;
    }
    if (Object.hasOwn(value, "$ref")) {
      if (typeof value.$ref !== "string" || !value.$ref) {
        throw new Error(`schema ${resource.id} contains a non-string or empty $ref`);
      }
      const resolved = new URL(value.$ref, nextBase);
      resolved.hash = "";
      if (resolved.href !== resource.id) references.add(resolved.href);
    }
    for (const key of Object.keys(value).sort()) walk(value[key], nextBase);
  }

  walk(resource.document, resource.id);
  return [...references].sort();
}

function u64be(value) {
  const encoded = Buffer.alloc(8);
  encoded.writeBigUInt64BE(BigInt(value));
  return encoded;
}

export function schemaBundleSha256(rootId, resources) {
  const bundle = schemaBundle(rootId, resources);
  const hash = createHash("sha256");
  hash.update(BUNDLE_DOMAIN);
  hash.update(u64be(bundle.length));
  for (const resource of bundle) {
    const id = Buffer.from(resource.id, "utf8");
    const sourceDigest = createHash("sha256").update(resource.source).digest();
    hash.update(u64be(id.length));
    hash.update(id);
    hash.update(u64be(sourceDigest.length));
    hash.update(sourceDigest);
  }
  return hash.digest("hex");
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  const directory = resolve(import.meta.dirname, "..");
  const resources = await loadSchemaResources(directory, [
    "common.schema.json",
    "space.schema.json",
    "capability.schema.json",
    "recipe.schema.json"
  ]);
  for (const id of [
    "https://doppels.so/schemas/v1alpha1/capability.schema.json",
    "https://doppels.so/schemas/v1alpha1/recipe.schema.json"
  ]) {
    console.log(`${id} ${schemaBundleSha256(id, resources)}`);
  }
}
