import { readdir, readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { isDeepStrictEqual } from "node:util";
import { createHash } from "node:crypto";
import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";
import { parse } from "yaml";
import { schemaBundleSha256, schemaResource } from "./schema-bundle.mjs";

const root = resolve(import.meta.dirname, "..");
const examples = resolve(root, "examples");
const schemaFiles = [
  "common.schema.json",
  "space.schema.json",
  "capability.schema.json",
  "recipe.schema.json",
  "request.schema.json",
  "run.schema.json",
  "run-event.schema.json",
  "share.schema.json",
  "share-created.schema.json",
  "share-message.schema.json"
];

function sha256(source) {
  return createHash("sha256").update(source).digest("hex");
}

const ajv = new Ajv2020({ allErrors: true, allowUnionTypes: true, strict: true });
addFormats(ajv);
const schemaDigests = new Map();
const schemaResources = new Map();
for (const file of schemaFiles) {
  const source = await readFile(resolve(root, file));
  const resource = schemaResource(source);
  const schema = resource.document;
  schemaResources.set(resource.id, resource);
  ajv.addSchema(schema);
}
for (const id of schemaResources.keys()) {
  schemaDigests.set(id, schemaBundleSha256(id, schemaResources));
}

const schemaForKind = new Map([
  ["Space", "space.schema.json"],
  ["Capability", "capability.schema.json"],
  ["Recipe", "recipe.schema.json"],
  ["Request", "request.schema.json"],
  ["Run", "run.schema.json"],
  ["RunEvent", "run-event.schema.json"],
  ["Share", "share.schema.json"],
  ["ShareCreated", "share-created.schema.json"],
  ["ShareMessage", "share-message.schema.json"]
]);
const validators = new Map(
  [...schemaForKind].map(([kind, file]) => [
    kind,
    ajv.getSchema(`https://doppels.so/schemas/v1alpha1/${file}`)
  ])
);

let failed = false;
function fail(message) {
  failed = true;
  console.error(message);
}

function schemaErrors(file, validate) {
  fail(`${file} is invalid:`);
  for (const error of validate.errors ?? []) {
    console.error(`  ${error.instancePath || "/"} ${error.message}`);
  }
}

function revisionKey(reference) {
  return `${reference.name}@${reference.version}`;
}

function definitionReferenceErrors(reference, revision, schemaId) {
  const errors = [];
  if (!revision) return ["references an unavailable definition revision"];
  if (reference.manifestSha256 !== revision.digest) {
    errors.push(`manifestSha256 does not match ${revisionKey(reference)}`);
  }
  if (reference.schema.id !== schemaId) {
    errors.push(`schema id must be ${schemaId}`);
  } else if (reference.schema.sha256 !== schemaDigests.get(schemaId)) {
    errors.push(`schema SHA-256 does not match ${schemaId}`);
  }
  return errors;
}

function scalarMatches(value, type) {
  return {
    string: typeof value === "string",
    integer: Number.isSafeInteger(value),
    number: typeof value === "number" && Number.isFinite(value) &&
      (!Number.isInteger(value) || Number.isSafeInteger(value)),
    boolean: typeof value === "boolean"
  }[type] ?? false;
}

for (const [value, type, expected] of [
  [-Number.MAX_SAFE_INTEGER, "integer", true],
  [Number.MAX_SAFE_INTEGER, "integer", true],
  [Number.MAX_SAFE_INTEGER + 1, "integer", false],
  [Number.MAX_SAFE_INTEGER + 1, "number", false],
  [1.5, "number", true]
]) {
  if (scalarMatches(value, type) !== expected) {
    fail(`scalar portability invariant failed for ${type} ${String(value)}`);
  }
}

function inputValueMatches(value, contract) {
  return scalarMatches(value, contract.type) && (!contract.enum || contract.enum.includes(value));
}

function inputSignature(contract) {
  return JSON.stringify({
    type: contract.type,
    required: contract.required ?? false,
    default: contract.default ?? null,
    enum: contract.enum ?? null
  });
}

function outputSignature(contract) {
  return JSON.stringify({ type: contract.type, mediaType: contract.mediaType ?? null });
}

const documents = [];
for (const file of (await readdir(examples)).sort()) {
  if (!file.endsWith(".yaml") && !file.endsWith(".json")) continue;
  const source = await readFile(resolve(examples, file), "utf8");
  const document = file.endsWith(".yaml") ? parse(source) : JSON.parse(source);
  const validate = validators.get(document.kind);
  if (!validate) {
    fail(`${file} has unsupported MVP kind ${String(document.kind)}`);
    continue;
  }
  if (!validate(document)) {
    schemaErrors(file, validate);
    continue;
  }
  documents.push({ file, document, digest: sha256(source) });
  console.log(`${file} is valid`);
}

const capabilityRevisions = new Map();
const capabilitiesByName = new Map();
for (const { file, document, digest } of documents.filter(({ document }) => document.kind === "Capability")) {
  const key = revisionKey(document.metadata);
  if (capabilityRevisions.has(key)) fail(`${file} duplicates Capability revision ${key}`);
  capabilityRevisions.set(key, { file, document, digest });
  const revisions = capabilitiesByName.get(document.metadata.name) ?? [];
  revisions.push({ file, document, digest });
  capabilitiesByName.set(document.metadata.name, revisions);

  for (const [inputName, input] of Object.entries(document.inputs)) {
    for (const value of input.enum ?? []) {
      if (!scalarMatches(value, input.type)) {
        fail(`${file}: input ${inputName} has an enum value incompatible with type ${input.type}`);
      }
    }
    if ("default" in input && !inputValueMatches(input.default, input)) {
      fail(`${file}: input ${inputName} has a default incompatible with its contract`);
    }
  }
}

function resolveCapabilityForRecipe(name, errors) {
  const revisions = capabilitiesByName.get(name) ?? [];
  if (revisions.length === 0) {
    errors.push(`provides unknown Capability ${name}`);
    return undefined;
  }
  if (revisions.length > 1) {
    errors.push(`Capability ${name} has multiple revisions; keep exactly one active revision in the Project for the MVP`);
    return undefined;
  }
  return revisions[0].document;
}

const identifier = "[a-z][a-z0-9]*(?:[-_][a-z0-9]+)*";
const inputReference = new RegExp(`^inputs\\.(${identifier})$`);
const stepReference = new RegExp(`^steps\\.(${identifier})\\.(${identifier})$`);

function templateErrors(value, location, inputNames, availableResults) {
  if (typeof value !== "string") return [];
  const errors = [];
  if (value.includes("${{")) errors.push(`${location} uses obsolete \${{ ... }} syntax`);

  const matches = [...value.matchAll(/\{\{([\s\S]*?)\}\}/g)];
  const remainder = value.replace(/\{\{[\s\S]*?\}\}/g, "");
  if (remainder.includes("{{") || remainder.includes("}}")) {
    errors.push(`${location} contains an incomplete expression`);
  }

  for (const match of matches) {
    const expression = match[1].trim();
    const input = inputReference.exec(expression);
    if (input) {
      if (!inputNames.has(input[1])) {
        errors.push(`${location} references input ${input[1]} not available in every provided Capability`);
      }
      continue;
    }

    const step = stepReference.exec(expression);
    if (step) {
      if (!availableResults.get(step[1])?.has(step[2])) {
        errors.push(`${location} references unavailable result ${step[1]}.${step[2]}`);
      }
      continue;
    }
    errors.push(`${location} contains unsupported expression {{ ${expression} }}`);
  }
  return errors;
}

function environmentErrors(environment, location, declaredHostEnv, inputNames, availableResults) {
  const errors = [];
  for (const [name, value] of Object.entries(environment ?? {})) {
    if (typeof value === "string") {
      errors.push(...templateErrors(value, `${location}.${name}`, inputNames, availableResults));
    } else if (value.from === "host_env" && !declaredHostEnv.has(value.name)) {
      errors.push(`${location}.${name} reads undeclared host environment variable ${value.name}`);
    }
  }
  return errors;
}

function scriptErrors(script, location) {
  const errors = [];
  if (script.includes("${{")) errors.push(`${location} uses obsolete \${{ ... }} syntax`);
  if (/\{\{\s*(inputs|steps)\./.test(script)) {
    errors.push(`${location} must bind Doppels values through Step env, not interpolate them into shell source`);
  }
  return errors;
}

function recipeErrors(recipe) {
  const errors = [];
  const capabilities = recipe.provides
    .map((name) => resolveCapabilityForRecipe(name, errors))
    .filter(Boolean);

  const commonInputs = new Set();
  if (capabilities.length > 0) {
    const allInputNames = new Set(capabilities.flatMap((capability) => Object.keys(capability.inputs)));
    for (const name of allInputNames) {
      const contracts = capabilities.map((capability) => capability.inputs[name]);
      if (contracts.every(Boolean)) {
        if (contracts.every((contract) => inputSignature(contract) === inputSignature(contracts[0]))) {
          commonInputs.add(name);
        } else {
          errors.push(`provided Capabilities define incompatible input ${name}`);
        }
      }
    }
  }

  const outputContracts = new Map();
  for (const capability of capabilities) {
    for (const [name, contract] of Object.entries(capability.outputs)) {
      if (outputContracts.has(name) && outputSignature(outputContracts.get(name)) !== outputSignature(contract)) {
        errors.push(`provided Capabilities define incompatible output ${name}`);
      }
      outputContracts.set(name, contract);
    }
  }

  if (recipe.runtime === "manual") return errors;

  const availableResults = new Map();
  const declaredHostEnv = new Set(recipe.requires?.hostEnv ?? []);
  const defaultApproval = recipe.defaults?.approval;

  errors.push(...environmentErrors(recipe.env, "env", declaredHostEnv, commonInputs, availableResults));
  if (recipe.defaults?.workingDirectory) {
    errors.push(...templateErrors(
      recipe.defaults.workingDirectory,
      "defaults.workingDirectory",
      commonInputs,
      availableResults
    ));
  }
  for (const [index, file] of (recipe.requires?.files ?? []).entries()) {
    errors.push(...templateErrors(file, `requires.files[${index}]`, commonInputs, availableResults));
  }

  for (const step of recipe.steps) {
    if (availableResults.has(step.id)) {
      errors.push(`duplicate Step id ${step.id}`);
      continue;
    }
    if (!step.approval && !defaultApproval) {
      errors.push(`Step ${step.id} must resolve approval from the Step or defaults.approval`);
    }
    errors.push(...environmentErrors(
      step.env,
      `steps.${step.id}.env`,
      declaredHostEnv,
      commonInputs,
      availableResults
    ));
    if (step.workingDirectory) {
      errors.push(...templateErrors(
        step.workingDirectory,
        `steps.${step.id}.workingDirectory`,
        commonInputs,
        availableResults
      ));
    }
    errors.push(...scriptErrors(step.run.script, `steps.${step.id}.run.script`));
    for (const [name, product] of Object.entries(step.produces ?? {})) {
      if (product.file) {
        errors.push(...templateErrors(
          product.file,
          `steps.${step.id}.produces.${name}.file`,
          commonInputs,
          availableResults
        ));
      }
    }
    availableResults.set(step.id, new Set(Object.keys(step.produces ?? {})));
  }

  for (const [name, value] of Object.entries(recipe.returns)) {
    errors.push(...templateErrors(value, `returns.${name}`, commonInputs, availableResults));
  }
  for (const name of outputContracts.keys()) {
    if (!(name in recipe.returns)) errors.push(`returns is missing Capability output ${name}`);
  }
  for (const [name, expression] of Object.entries(recipe.returns)) {
    const output = outputContracts.get(name);
    if (!output) {
      errors.push(`returns exposes undeclared Capability output ${name}`);
      continue;
    }
    const match = new RegExp(`^\\{\\{\\s*steps\\.(${identifier})\\.(${identifier})\\s*\\}\\}$`).exec(expression);
    if (!match) continue;
    const step = recipe.steps.find(({ id }) => id === match[1]);
    const product = step?.produces?.[match[2]];
    if (product && output.type === "artifact" && !product.file) {
      errors.push(`returns.${name} must reference a file for artifact output`);
    }
    if (product && output.type !== "artifact" && !product.env) {
      errors.push(`returns.${name} must reference an env value for scalar output`);
    }
  }
  return errors;
}

const recipeRevisions = new Map();
for (const { file, document, digest } of documents.filter(({ document }) => document.kind === "Recipe")) {
  const key = revisionKey(document.metadata);
  if (recipeRevisions.has(key)) fail(`${file} duplicates Recipe revision ${key}`);
  recipeRevisions.set(key, { file, document, digest });
  for (const error of recipeErrors(document)) fail(`${file}: ${error}`);
}

function validateRequestInputs(file, request, capability) {
  for (const [name, contract] of Object.entries(capability.inputs)) {
    if (contract.required && !(name in request.inputs) && !("default" in contract)) {
      fail(`${file}: missing required input ${name}`);
    }
  }
  for (const [name, value] of Object.entries(request.inputs)) {
    const contract = capability.inputs[name];
    if (!contract) fail(`${file}: unknown input ${name}`);
    else if (!inputValueMatches(value, contract)) fail(`${file}: input ${name} violates its Capability contract`);
  }
}

const requests = new Map();
for (const { file, document } of documents.filter(({ document }) => document.kind === "Request")) {
  if (requests.has(document.id)) fail(`${file} duplicates Request ${document.id}`);
  requests.set(document.id, { file, document });
  const capabilityRevision = capabilityRevisions.get(revisionKey(document.capability));
  const capability = capabilityRevision?.document;
  if (!capabilityRevision) {
    fail(`${file}: references unavailable Capability revision ${revisionKey(document.capability)}`);
    continue;
  }
  for (const error of definitionReferenceErrors(
    document.capability,
    capabilityRevision,
    "https://doppels.so/schemas/v1alpha1/capability.schema.json"
  )) fail(`${file}: ${error}`);
  validateRequestInputs(file, document, capability);
}

const shares = new Map();
for (const { file, document } of documents.filter(({ document }) => document.kind === "ShareCreated")) {
  const share = document.share;
  if (shares.has(share.id)) fail(`${file} duplicates Share ${share.id}`);
  shares.set(share.id, { file, document: share });
  if (new Date(share.expiresAt) <= new Date(share.createdAt)) {
    fail(`${file}: expiresAt must follow createdAt`);
  }
  if (revisionKey(share.capabilityRevision) !== revisionKey(share.capability.metadata)) {
    fail(`${file}: Capability snapshot and revision identify different definitions`);
  }
  const capabilityRevision = capabilityRevisions.get(revisionKey(share.capabilityRevision));
  const capability = capabilityRevision?.document;
  for (const error of definitionReferenceErrors(
    share.capabilityRevision,
    capabilityRevision,
    "https://doppels.so/schemas/v1alpha1/capability.schema.json"
  )) fail(`${file}: ${error}`);
  if (capability && (
    !isDeepStrictEqual(capability.inputs, share.capability.inputs) ||
    !isDeepStrictEqual(capability.outputs, share.capability.outputs)
  )) {
    fail(`${file}: Share snapshot changes the Capability contract`);
  }
  if (capability) {
    for (const [name, value] of Object.entries(share.capability.metadata)) {
      if (!isDeepStrictEqual(value, capability.metadata[name])) {
        fail(`${file}: Share snapshot changes Capability metadata ${name}`);
      }
    }
    if (share.capability.documentation && !isDeepStrictEqual(
      share.capability.documentation,
      capability.documentation
    )) {
      fail(`${file}: Share snapshot changes Capability documentation`);
    }
  }
  if (share.recipe) {
    const recipeRevision = recipeRevisions.get(revisionKey(share.recipe));
    const recipe = recipeRevision?.document;
    if (!recipeRevision) fail(`${file}: references unavailable Recipe revision ${revisionKey(share.recipe)}`);
    else if (!recipe.provides.includes(share.capability.metadata.name)) {
      fail(`${file}: shared Recipe does not provide Capability ${share.capability.metadata.name}`);
    }
    for (const error of definitionReferenceErrors(
      share.recipe,
      recipeRevision,
      "https://doppels.so/schemas/v1alpha1/recipe.schema.json"
    )) fail(`${file}: ${error}`);
  }
}

const requestsPerShare = new Map();
for (const { file, document } of requests.values()) {
  if (!document.shareId) continue;
  const share = shares.get(document.shareId)?.document;
  if (!share) {
    fail(`${file}: references unavailable Share ${document.shareId}`);
    continue;
  }
  if (!isDeepStrictEqual(document.capability, share.capabilityRevision)) {
    fail(`${file}: does not preserve its Share Capability revision`);
  }
  if (new Date(document.createdAt) < new Date(share.createdAt) || new Date(document.createdAt) > new Date(share.expiresAt)) {
    fail(`${file}: was created outside the Share lifetime`);
  }
  const count = (requestsPerShare.get(document.shareId) ?? 0) + 1;
  requestsPerShare.set(document.shareId, count);
  if (count > share.requestLimit) fail(`${file}: exceeds the Share request limit`);
}

const runs = new Map();
for (const { file, document } of documents.filter(({ document }) => document.kind === "Run")) {
  if (runs.has(document.id)) fail(`${file} duplicates Run ${document.id}`);
  runs.set(document.id, { file, document });
  const request = requests.get(document.requestId)?.document;
  if (!request) {
    fail(`${file}: references unavailable Request ${document.requestId}`);
    continue;
  }
  if (!isDeepStrictEqual(request.capability, document.capability) || !isDeepStrictEqual(request.inputs, document.inputs)) {
    fail(`${file}: does not preserve its Request Capability revision, digests and inputs`);
  }
  if (new Date(document.createdAt) < new Date(request.createdAt)) {
    fail(`${file}: predates its Request`);
  }

  let recipe;
  if (document.recipe) {
    const recipeRevision = recipeRevisions.get(revisionKey(document.recipe));
    recipe = recipeRevision?.document;
    if (!recipeRevision) fail(`${file}: references unavailable Recipe revision ${revisionKey(document.recipe)}`);
    else if (!recipe.provides.includes(document.capability.name)) {
      fail(`${file}: Recipe does not provide Capability ${document.capability.name}`);
    }
    for (const error of definitionReferenceErrors(
      document.recipe,
      recipeRevision,
      "https://doppels.so/schemas/v1alpha1/recipe.schema.json"
    )) fail(`${file}: ${error}`);
  }
  if (recipe?.runtime === "shell" && !document.nodeId) fail(`${file}: shell Run requires a Node`);
  if ((!recipe || recipe.runtime === "manual") && document.nodeId) {
    fail(`${file}: human Run must not claim an execution Node`);
  }

  if (request.shareId) {
    const share = shares.get(request.shareId)?.document;
    if (share?.recipe && !isDeepStrictEqual(share.recipe, document.recipe)) {
      fail(`${file}: does not preserve the Recipe revision selected by its Share`);
    }
  }
}

function returnMatches(value, contract) {
  if (contract.type !== "artifact") return scalarMatches(value, contract.type);
  const artifact = value?.artifact;
  return Boolean(artifact) && (!contract.mediaType || artifact.mediaType === contract.mediaType);
}

const eventsByRun = new Map();
for (const { file, document } of documents.filter(({ document }) => document.kind === "RunEvent")) {
  const run = runs.get(document.runId)?.document;
  if (!run) {
    fail(`${file}: references unavailable Run ${document.runId}`);
    continue;
  }
  if (new Date(document.occurredAt) < new Date(run.createdAt)) fail(`${file}: predates its Run`);
  const events = eventsByRun.get(document.runId) ?? [];
  if (events.some((event) => event.document.sequence === document.sequence)) {
    fail(`${file}: duplicates RunEvent sequence ${document.sequence}`);
  }
  events.push({ file, document });
  eventsByRun.set(document.runId, events);

  if (document.stepId) {
    const recipe = run.recipe && recipeRevisions.get(revisionKey(run.recipe))?.document;
    if (!recipe || recipe.runtime !== "shell" || !recipe.steps.some(({ id }) => id === document.stepId)) {
      fail(`${file}: references unavailable Step ${document.stepId}`);
    }
  }

  if (document.type === "run_succeeded") {
    const capability = capabilityRevisions.get(revisionKey(run.capability))?.document;
    if (!capability) continue;
    const returned = document.data.returns;
    for (const [name, contract] of Object.entries(capability.outputs)) {
      if (!(name in returned)) fail(`${file}: result is missing Capability output ${name}`);
      else if (!returnMatches(returned[name], contract)) fail(`${file}: result ${name} violates its output contract`);
    }
    for (const name of Object.keys(returned)) {
      if (!(name in capability.outputs)) fail(`${file}: result exposes undeclared Capability output ${name}`);
    }

    const recipe = run.recipe && recipeRevisions.get(revisionKey(run.recipe))?.document;
    if (recipe?.runtime === "manual") {
      const evidence = document.data.evidence ?? {};
      for (const [name, contract] of Object.entries(recipe.evidence)) {
        if (!(name in evidence)) fail(`${file}: result is missing manual evidence ${name}`);
        else if (contract.type === "string" && typeof evidence[name] !== "string") {
          fail(`${file}: evidence ${name} must be a string`);
        } else if (contract.type === "artifact" && !evidence[name]?.artifact) {
          fail(`${file}: evidence ${name} must be an artifact`);
        }
      }
      for (const name of Object.keys(evidence)) {
        if (!(name in recipe.evidence)) fail(`${file}: result exposes undeclared manual evidence ${name}`);
      }
    }
  }
}

const terminalEvents = new Set(["run_succeeded", "run_failed", "run_cancelled", "run_interrupted"]);
for (const [runId, events] of eventsByRun) {
  events.sort((a, b) => a.document.sequence - b.document.sequence);
  for (const [index, event] of events.entries()) {
    if (event.document.sequence !== index) {
      fail(`${event.file}: Run ${runId} event sequence must be contiguous from zero`);
    }
    if (index > 0 && new Date(event.document.occurredAt) < new Date(events[index - 1].document.occurredAt)) {
      fail(`${event.file}: RunEvent timestamps must be monotonic`);
    }
  }
  if (events[0]?.document.type !== "run_created") {
    fail(`${events[0].file}: first RunEvent must be run_created`);
  }
  const terminal = events.filter(({ document }) => terminalEvents.has(document.type));
  if (terminal.length > 1) fail(`${terminal[1].file}: Run ${runId} has more than one terminal event`);
  if (terminal.length === 1 && terminal[0] !== events.at(-1)) {
    fail(`${events.at(-1).file}: Run ${runId} contains an event after termination`);
  }
}

const messageIds = new Set();
for (const { file, document } of documents.filter(({ document }) => document.kind === "ShareMessage")) {
  if (messageIds.has(document.messageId)) fail(`${file}: duplicates ShareMessage ${document.messageId}`);
  messageIds.add(document.messageId);
  if (!shares.has(document.shareId)) fail(`${file}: references unavailable Share ${document.shareId}`);
  if (document.event === "request_available") {
    if (document.payload.shareId !== document.shareId) fail(`${file}: payload belongs to another Share`);
    const request = requests.get(document.payload.id)?.document;
    if (!request || !isDeepStrictEqual(request, document.payload)) {
      fail(`${file}: payload does not match the persisted Request`);
    }
  }
  if (document.event === "run_submitted" || document.event === "run_recorded") {
    const request = requests.get(document.payload.requestId)?.document;
    if (!request || request.shareId !== document.shareId) {
      fail(`${file}: Run payload does not belong to the envelope Share`);
    } else if (
      !isDeepStrictEqual(request.capability, document.payload.capability) ||
      !isDeepStrictEqual(request.inputs, document.payload.inputs)
    ) {
      fail(`${file}: Run payload changes its Request contract or inputs`);
    }
    const run = runs.get(document.payload.id)?.document;
    if (document.event === "run_recorded" && (!run || !isDeepStrictEqual(run, document.payload))) {
      fail(`${file}: payload does not match the persisted Run`);
    }
  }
  if (document.event === "run_event_submitted" || document.event === "run_event_recorded") {
    const run = runs.get(document.payload.runId)?.document;
    const request = run && requests.get(run.requestId)?.document;
    if (!run || !request || request.shareId !== document.shareId) {
      fail(`${file}: RunEvent payload does not belong to the envelope Share`);
    }
    const events = eventsByRun.get(document.payload.runId) ?? [];
    const event = events.find(({ document: candidate }) => candidate.sequence === document.payload.sequence)?.document;
    if (document.event === "run_event_recorded" && (!event || !isDeepStrictEqual(event, document.payload))) {
      fail(`${file}: payload does not match the persisted RunEvent`);
    }
  }
}

function expectSchemaRejection(name, candidate, validate) {
  if (validate(candidate)) fail(`${name} should have been rejected by its schema`);
}

function expectSemanticRejection(name, candidate, check) {
  if (check(candidate).length === 0) fail(`${name} should have been rejected semantically`);
}

const baseRecipe = documents.find(({ document }) => document.kind === "Recipe" && document.runtime === "shell")?.document;
const baseCapability = documents.find(({ document }) => document.kind === "Capability")?.document;
const baseSpace = documents.find(({ document }) => document.kind === "Space")?.document;
const baseRun = documents.find(({ document }) => document.kind === "Run")?.document;
const baseShareCreated = documents.find(({ document }) => document.kind === "ShareCreated")?.document;
const baseSucceeded = documents.find(({ document }) => document.kind === "RunEvent" && document.type === "run_succeeded")?.document;

if (!baseRecipe || !baseCapability || !baseSpace || !baseRun || !baseShareCreated || !baseSucceeded) {
  fail("validator fixtures are incomplete");
} else {
  expectSchemaRejection(
    "Recipe-owned inputs",
    { ...structuredClone(baseRecipe), inputs: {} },
    validators.get("Recipe")
  );
  expectSchemaRejection(
    "Capability with Steps",
    { ...structuredClone(baseCapability), steps: [] },
    validators.get("Capability")
  );
  expectSchemaRejection(
    "versioned Space",
    {
      ...structuredClone(baseSpace),
      metadata: { ...structuredClone(baseSpace.metadata), version: "1.0.0" }
    },
    validators.get("Space")
  );
  expectSchemaRejection(
    "Organization-bound Space",
    { ...structuredClone(baseSpace), organization: "acme" },
    validators.get("Space")
  );
  expectSchemaRejection(
    "Space enumerating definitions",
    { ...structuredClone(baseSpace), capabilities: ["release-preview"] },
    validators.get("Space")
  );
  expectSchemaRejection(
    "manual Recipe with shell fields",
    {
      ...structuredClone(baseRecipe),
      runtime: "manual",
      procedure: { readme: "RUNBOOK.md" },
      evidence: { notes: { type: "string" } }
    },
    validators.get("Recipe")
  );
  expectSchemaRejection(
    "obsolete expression",
    {
      ...structuredClone(baseRecipe),
      returns: Object.fromEntries(Object.keys(baseRecipe.returns).map((key) => [key, "${{ steps.fake.result }}"]))
    },
    validators.get("Recipe")
  );

  const noApproval = structuredClone(baseRecipe);
  delete noApproval.defaults.approval;
  for (const step of noApproval.steps) delete step.approval;
  expectSemanticRejection("implicit approval", noApproval, recipeErrors);

  const forwardReference = structuredClone(baseRecipe);
  forwardReference.steps[0].env = { LATER: `{{ steps.${forwardReference.steps.at(-1).id}.missing }}` };
  expectSemanticRejection("forward Step reference", forwardReference, recipeErrors);

  const unknownScriptInput = structuredClone(baseRecipe);
  unknownScriptInput.steps[0].run.script += "\necho '{{ inputs.unknown }}'\n";
  expectSemanticRejection("unknown script input", unknownScriptInput, recipeErrors);

  const undeclaredHostEnv = structuredClone(baseRecipe);
  undeclaredHostEnv.env = { TOKEN: { from: "host_env", name: "UNDECLARED_TOKEN" } };
  expectSemanticRejection("undeclared host environment", undeclaredHostEnv, recipeErrors);

  const incompatibleCapabilities = structuredClone(baseRecipe);
  incompatibleCapabilities.provides = [...baseRecipe.provides, "service-status"];
  expectSemanticRejection(
    "input unavailable across multiple Capabilities",
    incompatibleCapabilities,
    recipeErrors
  );

  const guestRun = structuredClone(baseRun);
  guestRun.executor = { kind: "guest", id: "link-guest" };
  expectSchemaRejection("guest executor", guestRun, validators.get("Run"));

  const tamperedReference = structuredClone(baseRun.capability);
  tamperedReference.manifestSha256 = "0".repeat(64);
  const referencedCapability = capabilityRevisions.get(revisionKey(tamperedReference));
  if (definitionReferenceErrors(
    tamperedReference,
    referencedCapability,
    "https://doppels.so/schemas/v1alpha1/capability.schema.json"
  ).length === 0) {
    fail("tampered manifest digest should have been rejected semantically");
  }

  const manualShare = structuredClone(baseShareCreated);
  delete manualShare.share.recipe;
  if (!validators.get("ShareCreated")(manualShare)) {
    fail("Share without Recipe should remain valid");
  }

  const missingResult = structuredClone(baseSucceeded);
  delete missingResult.data.returns[Object.keys(missingResult.data.returns)[0]];
  const run = runs.get(missingResult.runId)?.document;
  const capability = run && capabilityRevisions.get(revisionKey(run.capability))?.document;
  if (capability) {
    const errors = [];
    for (const name of Object.keys(capability.outputs)) {
      if (!(name in missingResult.data.returns)) errors.push(name);
    }
    if (errors.length === 0) fail("missing Capability result should have been rejected semantically");
  }
}

console.log("Playbook is intentionally outside the MVP schema set");
if (failed) process.exitCode = 1;
