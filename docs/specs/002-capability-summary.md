# Implementation Specification: Automatically Generated Hardware Capability Summaries

**Project:** `rh-ecosystem-edge/k8s-dra-topology-coordinator`
**Feature:** Derived Hardware Capability Summaries
**Status:** Implementation is blocked on the aggregate-capacity correction in Readiness blocker 1.
**Prerequisites:** Intent-Based Hardware Offerings metadata enhancement (PR #13) and aggregate count/capacity fixes (PR #14) are merged to `main`. Verify those changes and the additional correction in Readiness blocker 1 are present on the implementation base.
**Initial scope:** Kubernetes Dynamic Resource Allocation (DRA) DeviceClasses
**Priority:** High

---

## Amendment note (read this first)

This spec has been reviewed against the current code, including a separate architecture pass. Review findings and decisions are recorded below. Amendments are marked `[AMENDED]` inline.

### Readiness blockers

1. **Not yet resolved:** aggregate capacity reduction does not preserve missing-key state across all alternatives, and malformed quantities have no valid minimum. See "Code fix 2" below. Do not treat aggregate capacity as guaranteed until the reducer retains absent/invalid state across the full fold and rejects an aggregate whose quantities cannot be compared. Confirmed with a reproducing test against `main`: a capacity key dropped because one alternative lacked it can be silently reintroduced by a later alternative that happens to declare it again — see the fix PR once opened.
2. ~~The 32 KiB summary-pair limit does not ensure the final merged annotation map stays below Kubernetes' 256 KiB combined limit.~~ **Resolved by design decision** — see "Annotation budget" below and the amended FR-2. The check lives in `publishDeviceClass`, is feature-agnostic, and drops Topology Coordinator (TC)-owned annotations in a documented expendability order.
3. ~~The `unchanged` placeholder sections and the 22-case test inventory are missing.~~ **Resolved** — every placeholder section has been restored verbatim from the original spec draft (§5, FR-4, FR-6, FR-8, FR-10, FR-11, §9, §10, §11, §13), and the 22-case unit-test inventory in §8 is enumerated rather than referenced by count.

### Code fix 1: grouping aggregates did not intersect device counts

The original §6 FR-5 assumed "the final PartitionConfig associated with each generated class" was always a trustworthy, guaranteed-across-every-realization description for aggregate classes. It was not, for one specific aggregate type.

Verified in `internal/controller/deviceclass_manager.go`: `SyncDeviceClasses`'s partition-type aggregate (the pcieroot/numa aggregate, and by inheritance the tier-named aggregates) intersects driver counts — a driver's count only survives if it's present on *every* partition of that type (`driverSeen[driver] == total`), at the minimum count across instances, with a System Locality Information Table (SLIT) distance-based reachability carve-out for NUMA-shareable drivers. This establishes a count guarantee; it does not establish that capacity values are identical or conservative across those partitions.

`SyncGroupingDeviceClasses`'s grouping aggregate (the one named after the grouping itself, e.g. `gpu-nic-pair`) did **not** do this. It picked one arbitrary instance — deterministically the lowest `RailIndex`, as of a prior correctness fix — and published that single instance's `PartitionConfig` as the entire aggregate's config. If the grouping is asymmetric across rails (devices present on some rails but not others — explicitly the reason groupings exist, per the code's own comment on the partition-aggregate intersection: "Asymmetric devices... handled via grouping DeviceClasses instead"), the grouping aggregate's capability summary would describe one rail's hardware as if it were guaranteed for the whole grouping. A pure `BuildCapabilitySummary(config PartitionConfig) (CapabilitySummary, error)` has no way to detect or correct this from inside the function — `PartitionConfig` carries no provenance about whether it was intersected or arbitrarily picked, so FR-5's "if PartitionConfig does not contain enough information to distinguish alternatives, report that limitation" could not have been implemented against the original code no matter how carefully `BuildCapabilitySummary` was written.

**Fix (PR #14, merged to `main`):** `SyncGroupingDeviceClasses` intersects driver presence and counts across instances of a grouping name, retaining the minimum count. The NUMA-SLIT-reachability carve-out was deliberately **not** ported — it's a partition-type-specific optimization tied to NUMA distance semantics with no equivalent in grouping alignment (pcieRoot/numaNode/socket are a matching key here, not a distance model). Regression test: `TestDeviceClassManager_SyncGroupingDeviceClasses_AggregateIntersectsAsymmetricRails`.

### Code fix 2: aggregate capacity was nondeterministic and unsafe

The count fix above left capacity untouched, and a follow-up review caught that this was not merely incomplete but actively broken — in both aggregate paths, including the partition-type path that predates all of this work.

Both accumulators assigned capacity with an unconditional overwrite (`existing.minCap[driver] = cap`), despite the field name. Last writer won, and the writer was chosen by Go map iteration order over `seen`. Reproduced before fixing: two PCIe-root partitions under one aggregate key with capacities `16` and `8`, synced 40 times, produced `16` on 5 runs and `8` on 35.

This is not a contrived input. `partition_builder.go` derives per-partition capacity as `divideQuantity(capVal, numRoots)` where `numRoots` is the PCIe-root count of that partition's NUMA node, so any node whose NUMA nodes have different root counts produces exactly this shape.

Two consequences, both real and both predating this feature:

- **Nondeterminism.** The aggregate's `Spec.Config` bytes changed between reconciles, so `publishDeviceClass`'s no-op short-circuit never fired for that class and it issued an Update every reconcile — defeating the idempotency work in PR #12.
- **Incorrectness.** Advertising the larger capacity means claims expanded against the aggregate request more than the smaller partition can supply, and fail to schedule there.

**Partial fix (PR #14, merged to `main`):** capacity values are reduced pairwise with `mergeMinCapacity`. Two gaps remain in the aggregate accumulator and must be fixed before capability summaries can rely on it:

- When an alternative lacks a capacity key, the accumulator can delete the empty intersection and later re-add that key from a subsequent alternative. Track absence as terminal state for the full aggregate fold; a key absent from any alternative must stay absent regardless of input order.
- `resource.Quantity.ParseQuantity` failures currently fall back to lexical order. Lexical order is deterministic but does not identify a numeric minimum. Reject aggregate construction when any contributing capacity value cannot be parsed; return a surfaced per-object error and continue reconciling other classes.

**Consequence for this spec — this supersedes the earlier "omit the summary when capacities differ" policy once the reducer gaps above are fixed.** With driver counts and valid capacity values intersected at the source, an aggregate's `PartitionConfig` can be trusted without adding provenance to the summary schema:

- `BuildCapabilitySummary(config PartitionConfig)` stays a pure function with no provenance or confidence parameter.
- No omit-on-divergence policy, no uncertainty field in the published schema.
- Per-instance summaries copy valid configured capacity strings unchanged; invalid values cause summary construction to fail.

On the specific question of *minimum* versus *exact equality*: minimum is correct for parseable quantities and is the same kind of guarantee as the minimum device count. If one partition supplies 16 and another 8, "8" is guaranteed everywhere and more informative than publishing nothing. Exact input quantity strings remain unchanged in the summary; see FR-8.

### Annotation budget (resolves readiness blocker 2)

Kubernetes caps the *combined* size of all annotations on an object at 256 KiB — `TotalAnnotationSizeLimitB` in `k8s.io/apimachinery/pkg/api/validation`, enforced as `sum(len(key) + len(value))` over the whole map. There is no per-key limit. A fixed per-feature byte budget therefore cannot guarantee compliance, because foreign annotations (kubectl's `last-applied-configuration`, Helm release metadata, ArgoCD tracking) are unbounded and unknown until publish time.

Three decisions, settling what the earlier amendment left open:

**1. Use the vendored validator, do not hand-roll the byte count.** `k8s.io/apimachinery/pkg/api/validation` is already vendored and listed in `vendor/modules.txt`, and exports `ValidateAnnotationsSize(map[string]string) error` — the same function the API server runs. Call it. A hand-rolled `len(k)+len(v)` loop would duplicate logic that can drift from upstream.

**2. The check belongs in `publishDeviceClass`, and it is feature-agnostic.** That function is the only place the final merged map exists — the builders never see foreign annotations, because those are only known after the `Get`. The coupling objection (teaching shared infrastructure about two capability-summary keys) is resolved by not doing that: `publishDeviceClass` consults a package-level ordered list of TC-owned annotation keys ranked by expendability, and drops from the front until `ValidateAnnotationsSize` passes. It learns nothing about "the capability feature"; expendability is a property of each annotation. A future TC annotation is added to the list and needs no change to the check.

**3. Precedence on overflow: drop derived data before authored data.** This is the principle, not an arbitrary pick:

| Order | Key | Why it drops first |
|---|---|---|
| 1 | `.../capabilities` | Largest, and fully reconstructible by any consumer from `Spec.Config` on the same object — it is a derivation of data sitting right next to it. |
| 2 | `.../capability-summary` | Also derived from `Spec.Config`, but small and human-facing, so it survives longer. |
| 3 | `.../workload-types`, `.../categories`, `.../description`, `.../display-name` | Administrator-authored. Reconstructible only by finding the source grouping ConfigMap, not from the object itself. Ordering within this tier is by descending expected size and only matters in a pathological case. |

Dropping continues down the list until the map validates. If even an empty TC annotation set does not fit, foreign annotations alone exceed the limit; publish the DeviceClass anyway and let the API server reject it with its own error, rather than inventing a failure mode — at that point the object is broken in a way TC did not cause and cannot fix.

The rationale for dropping *any* TC annotation rather than withholding the update: `Spec.Config` is the functional payload that claim expansion depends on, and annotations are metadata. A DeviceClass that reconciles correctly with less metadata is strictly better than one that cannot be updated at all.

**The 32 KiB per-feature bound stays, with a narrowed purpose.** It is not a correctness guarantee — the combined check above is. It is a sanity bound that keeps one feature's output from crowding out everything else on a normal object. If summary construction exceeds the bound, the integration call site adds the DeviceClass name to the returned error before omitting the annotation.

### Other amendments (smaller, non-blocking)

- **FR-2 / FR-12 annotation size — concrete bound added.** See amended FR-2 below.
- **Placement resolved: annotate both aggregate and per-instance DeviceClasses**, not aggregate-only. See amended §7 Integration below.
- **`TopologyCapability.Enforcement` should stay `EnforcementMode`, not `string`** — it's already a named type in this package (`internal/controller/topology_rules.go`) that marshals to a JSON string fine; widening it to `string` in the summary loses exhaustive-switch safety in `FormatCapabilitySummary` for no benefit. See amended §4 below.
- **FR-9 determinism — extend the shared canonicalizer instead of maintaining a second ordering.** See amended FR-9 below.

---

## 1. Objective

Extend the Topology Coordinator (TC) to automatically generate structured, machine-readable hardware capability summaries from the existing `PartitionConfig` used to generate and expand DRA DeviceClasses.

The purpose is to make hardware offerings easier to discover, understand, compare, and consume without requiring administrators to duplicate hardware specifications in user-facing metadata.

Today, `PartitionConfig` already defines:

- Underlying DeviceClasses.
- Device counts.
- Consumable capacity requests.
- Device selectors.
- Device partition modes.
- Cross-device topology alignment constraints.
- Constraint enforcement policies.

In this specification, “normalized” means canonical collection ordering and compact serialization. Capacity quantity strings remain verbatim; their units are not normalized.

This information should be used to generate a normalized capability summary published on the corresponding DeviceClass.

**Primary design principle: The existing PartitionConfig remains the source of truth. The capability summary is a derived, read-only representation of the hardware requirements.**

The capability summary must never independently control resource allocation.

---

## 2. Goals

The implementation must:

1. Generate capability summaries automatically from PartitionConfig.
2. Preserve detailed device requirements without inventing hardware capabilities.
3. Publish the summary on TC-managed DeviceClasses.
4. Support CPUs, memory, GPUs, NICs, and arbitrary DRA devices.
5. Support DRA consumable capacities.
6. Describe topology alignment requirements.
7. Support grouping and partition DeviceClasses.
8. Preserve unknown driver-specific resources.
9. Produce stable, deterministic output.
10. Make summaries suitable for future catalog UIs, CLIs, and provider integrations.
11. Preserve existing ResourceClaim expansion behavior.
12. Avoid introducing another controller or CRD.

## 3. Existing Data Model

```go
type PartitionConfig struct {
    Kind         string              `json:"kind"`
    SubResources []SubResourceConfig `json:"subResources"`
    Alignments   []AlignmentConfig   `json:"alignments"`
}

type SubResourceConfig struct {
    DeviceClass   string            `json:"deviceClass"`
    Count         int               `json:"count"`
    Capacity      map[string]string `json:"capacity,omitempty"`
    Selectors     []string          `json:"selectors,omitempty"`
    PartitionMode string            `json:"partitionMode,omitempty"`
}

type AlignmentConfig struct {
    Attribute   string          `json:"attribute"`
    Requests    []string        `json:"requests"`
    Enforcement EnforcementMode `json:"enforcement"`
}
```

The system must derive capabilities from the actual resolved PartitionConfig rather than reparsing the original administrator grouping ConfigMap.

This ensures the summary reflects the configuration TC uses for claim expansion.

**[AMENDED]** `PartitionConfig` is fully resolved — no live Kubernetes queries or ConfigMap re-reads — by the time it reaches `buildGroupingDeviceClass` or `buildDeviceClassFromCache`. Keep `BuildCapabilitySummary(config PartitionConfig)` pure: copy `config.Kind` to `CapabilitySummary.Kind`; do not infer a grouping, partition, or tier type from the DeviceClass name.

---

## 4. Proposed CapabilitySummary Structure

```go
type CapabilitySummary struct {
    SchemaVersion string                  `json:"schemaVersion"`
    Kind          string                  `json:"kind"`
    Resources     []ResourceCapability    `json:"resources"`
    Topology      []TopologyCapability    `json:"topology,omitempty"`
}

type ResourceCapability struct {
    DeviceClass   string            `json:"deviceClass"`
    Count         int               `json:"count"`
    Capacity      map[string]string `json:"capacity,omitempty"`
    PartitionMode string            `json:"partitionMode,omitempty"`
    Selectors     []string          `json:"selectors,omitempty"`
}

type TopologyCapability struct {
    Attribute   string          `json:"attribute"`
    Requests    []string        `json:"requests"`
    Enforcement EnforcementMode `json:"enforcement"` // [AMENDED] was `string` — reuse the existing named type (topology_rules.go), it already marshals to a JSON string correctly and this preserves exhaustive-switch safety in FormatCapabilitySummary.
}
```

### Design requirements

- `Resources` must describe the underlying DRA requests, not an assumed allocation result.
- `DeviceClass` must retain its full name.
- `Count` must preserve the actual request count semantics.
- `Capacity` must preserve the original qualified capacity names and values.
- `PartitionMode` must retain provider-specific partition information.
- `Selectors` must copy every selector from the corresponding `SubResourceConfig`, preserving the expression text and order.
- `Topology` must preserve the actual alignment constraints.
- `SchemaVersion` must allow future schema evolution.

`CapabilitySummary.Kind` copies `PartitionConfig.Kind` verbatim. It describes the config schema, not whether the DeviceClass represents a grouping, partition, or tier.

Do not infer a device type from its class name alone.

For example, a DeviceClass containing the word `gpu` does not guarantee the device is a GPU.

The initial summary should remain generic, even when the devices happen to be CPUs, GPUs, NICs, or memory.

### Important distinction

A `count` of `1` combined with a requested consumable capacity of `16` for `dra.cpu/cpu` does not mean the workload requests one CPU.

It may represent one grouped DRA device and a separate consumable-capacity request for that quantity.

Preserve both fields — do not collapse these into an inaccurate resource count.

---

## 5. Example Derived Summary

Suppose the resolved PartitionConfig contains:

```json
{
  "kind": "PartitionConfig",
  "subResources": [
    {
      "deviceClass": "dra.cpu",
      "count": 1,
      "capacity": {
        "dra.cpu/cpu": "16"
      }
    },
    {
      "deviceClass": "gpu.example.com",
      "count": 2
    },
    {
      "deviceClass": "rdma.example.com",
      "count": 1
    }
  ],
  "alignments": [
    {
      "attribute": "resource.kubernetes.io/numaNode",
      "requests": ["cpu", "gpu", "nic"],
      "enforcement": "required"
    }
  ]
}
```

The capability generator should create an equivalent representation with canonical collection ordering. It preserves quantity strings verbatim and does not normalize units:

```json
{
  "schemaVersion": "v1alpha1",
  "kind": "PartitionConfig",
  "resources": [
    {
      "deviceClass": "dra.cpu",
      "count": 1,
      "capacity": {
        "dra.cpu/cpu": "16"
      }
    },
    {
      "deviceClass": "gpu.example.com",
      "count": 2
    },
    {
      "deviceClass": "rdma.example.com",
      "count": 1
    }
  ],
  "topology": [
    {
      "attribute": "resource.kubernetes.io/numaNode",
      "requests": ["cpu", "gpu", "nic"],
      "enforcement": "required"
    }
  ]
}
```

This example illustrates the desired schema. Validate request references against the actual naming produced by TC; do not assume `cpu`, `gpu`, and `nic` are always the generated request names.

The resulting summary should be consistent with the ResourceClaim that TC ultimately generates.

This example describes a single concrete grouping, not an aggregate spanning alternative realizations, so it is unaffected by the aggregate intersection semantics discussed above.

---

## 6. Functional Requirements

### FR-1: Derive Summaries from PartitionConfig

```go
func BuildCapabilitySummary(
    config PartitionConfig,
) (CapabilitySummary, error)
```

**[AMENDED — confirms original intent, now actually buildable]** Because every `PartitionConfig` reaching this function is now trustworthy (intersected where it needs to be, concrete and non-heterogeneous everywhere else), this signature stays exactly as proposed — a pure function with no provenance/confidence parameter. This was flagged during architect review as a real risk if the aggregate fix hadn't landed first; it no longer is.

The implementation must:

- Read existing sub-resource definitions.
- Preserve all device counts.
- Preserve parseable consumable capacity values verbatim; return an error for an invalid quantity.
- Preserve device partition modes.
- Preserve every selector from each sub-resource, including its expression text and order.
- Preserve topology alignment requirements.
- Return a normalized summary.
- Avoid mutation of the input configuration.

Do not query Kubernetes during this transformation. Do not read the original ConfigMap during this transformation. Do not independently calculate hardware composition using ResourceSlices.

### FR-2: Publish Summary on DeviceClasses

**[AMENDED]** Original text below, with a concrete size bound and an explicit failure-mode decision added — the original left both as open principles ("respect Kubernetes annotation size limits", "define a clear failure behavior") without numbers or a decision.

Publish the serialized capability summary using an annotation on the generated DeviceClass:

```yaml
metadata:
  annotations:
    nodepartition.dra.k8s.io/capabilities: |
      {
        "schemaVersion": "v1alpha1",
        "kind": "PartitionConfig",
        "resources": [
          {
            "deviceClass": "gpu.example.com",
            "count": 2
          }
        ]
      }
```

Requirements:

- Use compact JSON for the actual stored annotation.
- Produce deterministic serialization.
- Preserve existing DeviceClass annotations.
- Enforce the 32 KiB feature budget on the combined `capabilities` and `capability-summary` key/value bytes before adding them to `dc.Annotations`.
- **Before each API create or update, validate the final merged annotation map with `validation.ValidateAnnotationsSize` from the vendored `k8s.io/apimachinery/pkg/api/validation`.** On updates, validate after every conflict retry fetches the latest object and rebuilds the merged annotations, immediately before the update. On failure, drop TC-owned annotations in the documented expendability order (`.../capabilities` first, then `.../capability-summary`, then the intent annotations) until the map validates, logging a surfaced error naming the DeviceClass and keys dropped, then publish the remaining metadata and `Spec.Config`.

  The check is feature-agnostic: it consults a package-level ordered list of TC-owned keys, not capability-summary-specific logic. See "Annotation budget" at the top of this document for the full rationale, the drop-order table, and why derived annotations drop before administrator-authored ones.
- **If summary construction fails for an oversized summary or an invalid per-instance capacity value, omit the summary annotations and log a surfaced error naming the DeviceClass; do not withhold that DeviceClass update.** Omitting the summary keys (and deleting stale values through `mergeTCOwnedMetadata`) still publishes the rest of the DeviceClass, including `Spec.Config`. Aggregate construction with invalid contributing quantities is rejected under FR-5.

### FR-3: Generate Human-Readable Summaries

In addition to the machine-readable JSON, generate a concise human-readable description via the `nodepartition.dra.k8s.io/capability-summary` annotation.

Requirements:

- Generate the text from the normalized capability summary.
- Do not read independent administrator-entered hardware descriptions.
- Use stable resource ordering.
- Keep text within a bounded length.
- Do not assume driver resource names imply specific physical hardware.
- Use accurate DRA terminology when capacity and device count differ.

**[AMENDED — implementation note, not a requirement change]** Use wording such as "requested consumable capacity `dra.cpu/cpu=16` (1 requested device), 2 × `gpu.example.com`...". Generate it from a generic per-resource template that shows both count and capacity whenever `Capacity` is non-empty; do not infer a physical device type from the class name.

### FR-4: Integrate with Existing Intent Metadata

The previous enhancement introduces intent-oriented metadata such as:

- `displayName`
- `description`
- `intentLevel`
- `categories`
- `workloadTypes`

The generated capability summary must complement, not replace, that metadata.

For example:

```yaml
metadata:
  annotations:
    nodepartition.dra.k8s.io/display-name: "AI Inference — Large"
    nodepartition.dra.k8s.io/description: "GPU compute intended for inference workloads"
    nodepartition.dra.k8s.io/capability-summary: "2 × gpu.example.com; requested consumable capacity dra.cpu/cpu=16"
    nodepartition.dra.k8s.io/capabilities: '{"schemaVersion":"v1alpha1",...}'
```

The administrator-authored description communicates intended use.

The generated capability summary communicates actual DRA requirements.

These two concepts must remain independent.

A description such as "Large AI Inference" must not automatically imply a GPU type, GPU memory quantity, or performance guarantee.

### FR-5: Support All TC DeviceClass Types

**[AMENDED — the blocker this whole amendment exists to resolve]**

Generate summaries for:

- Administrator-defined grouping classes.
- Automatically discovered grouping classes.
- PCIe-root partitions.
- NUMA partitions.
- Full-node partitions.
- Hardware-dependent tier classes.
- Profile-specific classes.

Use the final PartitionConfig associated with each generated class.

The following is a required invariant for every aggregate DeviceClass:

> If a class represents alternative physical realizations with different underlying configurations, do not publish one representative configuration as though it is guaranteed for every realization. Either (1) publish explicitly represented configuration variants, with their applicability, or (2) publish only characteristics guaranteed across every alternative. If the existing PartitionConfig does not contain enough information to establish that guarantee, block the summary for that class and report the limitation rather than inferring uniformity.

**Current status:** driver presence and counts are intersected. Capacity is not yet guaranteed until the accumulator and malformed-quantity gaps in "Code fix 2" are corrected. The implementation must satisfy option (2) — publish only characteristics guaranteed across every alternative — for every aggregate type:

- Partition-type aggregates (pcieroot/numa) and tier-named aggregates: intersect driver presence and counts, with an SLIT-reachability carve-out for NUMA-shareable drivers; capacity must retain only keys present in every alternative and reduce valid quantities to their per-key minimum.
- Grouping aggregates: apply the same capacity and count rules, without the NUMA-SLIT carve-out (no grouping-alignment equivalent).

Once the reducer enforces this invariant, `BuildCapabilitySummary` can trust its `PartitionConfig` input without a provenance parameter. **Do not add a `Representative`/`Guaranteed` boolean field to `CapabilitySummary`**; correct heterogeneous handling belongs in the aggregate configuration builder.

If a future aggregate type is added that cannot be made to intersect, the correct response is the one this spec has given from the start: block capability-summary annotation on that specific aggregate type until it can be fixed, rather than adding a permanent escape hatch to the published schema.

### FR-6: Preserve Arbitrary DRA Resources

The system must remain vendor-agnostic.

Examples of possible resource classes:

```text
gpu.example.com
rdma.example.com
dra.cpu
memory.example.com
fpga.example.com
accelerator.vendor.example
```

The summary must preserve unknown classes and capacity names without requiring hardcoded mappings.

Do not introduce a fixed CPU/GPU/NIC device enum into the core transformation.

Future semantic classification may be added using administrator-defined metadata or verified driver capabilities.

### FR-7: Topology Summary

Preserve topology requirements from `AlignmentConfig`: full qualified attribute name, participating generated request identifiers, required versus preferred enforcement, multiple simultaneous alignment constraints.

Do not infer PCIe-root or NUMA guarantees solely from a grouping's name. Do not turn a preferred constraint into a guaranteed capability. Do not describe fallback behavior as guaranteed unless the actual expanded configuration enforces it.

**[Confirmed, no change needed]** `EnforcementMode` already has exactly two values, `"required"` and `"preferred"` (`internal/controller/topology_rules.go`), matching this requirement's vocabulary directly — `BuildCapabilitySummary` just copies the field through (see §4 amendment: keep it typed, not stringified).

### FR-8: Capacity Semantics

Consumable DRA capacity values must be handled carefully.

Requirements:

- Preserve qualified capacity keys.
- Preserve exact capacity quantities.
- Parse quantities to compare alternatives; preserve the selected input string verbatim and do not normalize its units.
- Treat an unparseable quantity as invalid. `BuildCapabilitySummary` returns an error for it, and aggregate construction must fail if any contributing alternative contains one; do not use lexical ordering as a minimum.
- Avoid floating-point conversions.
- Do not confuse requested capacity with total device capacity.
- Do not confuse DeviceClass eligibility with current resource availability.
- Do not calculate aggregate capacity unless doing so is formally valid for that capacity model.

Examples:

```json
{
  "capacity": {
    "dra.cpu/cpu": "16"
  }
}
```

```json
{
  "capacity": {
    "memory.example.com/memory": "64Gi"
  }
}
```

The summary communicates what is requested.

It does not guarantee the scheduler can satisfy that request at the current moment.

**[AMENDED]** Aggregate reduction compares valid quantities with `resource.Quantity.Cmp` and selects one of the input strings verbatim. It does not sum, scale, or unit-convert values for display. Invalid inputs cannot be ordered into a trustworthy minimum and must fail aggregate construction.

### FR-9: Deterministic Output

**[AMENDED]**

Identical PartitionConfigs must generate identical summaries regardless of map iteration order.

Requirements:

- Sort resources using a stable, documented ordering with tie-breakers over every serialized field where ordering is not semantically meaningful.
- Sort capacity-map keys for serialization; JSON object key order is not semantically meaningful.
- Treat `AlignmentConfig.Requests` as a set of request identifiers and sort them lexically; retain request identity.
- Preserve ordering for every other list unless its semantics are explicitly documented as order-independent.
- Serialize consistently.
- Do not include timestamps or nondeterministic values.

Avoid unnecessary DeviceClass updates when nothing relevant has changed.

**[AMENDED]** `sortPartitionConfig` currently sorts sub-resources by `(DeviceClass, Count)` and alignments by `(Attribute, Requests)`. These keys do not fully order entries that differ in other serialized fields; alignment ordering also needs `Enforcement` as a tie-breaker. Extend one shared canonicalizer to deterministically order all serialized fields for order-independent lists, and use it for both config and summary generation. `BuildCapabilitySummary` must not rely on an undocumented call-site ordering invariant or maintain a second independent comparator. Preserve the order of lists whose order is semantically meaningful, including selectors.

### FR-10: Reconciliation

Integrate summary generation into the existing DeviceClass construction and publication path.

Expected behavior:

| Event | Result |
|---|---|
| PartitionConfig created | Capability summary generated |
| PartitionConfig changed | Summary updated |
| Device grouping modified | Summary reflects resulting configuration |
| Topology changes | Summary reflects the newly generated DeviceClass configuration |
| Controller restarted | Summary restored |
| No relevant changes | No unnecessary API update |
| Class removed | Existing cleanup behavior preserved |

The summary generator must not have its own reconciliation loop.

### FR-11: Discovery

Users should be able to discover capability information through standard Kubernetes APIs.

Example:

```bash
kubectl get deviceclass ai-inference-large \
  -o jsonpath='{.metadata.annotations.nodepartition\.dra\.k8s\.io/capability-summary}'
```

For the structured JSON:

```bash
kubectl get deviceclass ai-inference-large \
  -o json |
  jq -r '.metadata.annotations["nodepartition.dra.k8s.io/capabilities"] | fromjson'
```

Document commands that combine intent metadata with derived capabilities.

Verify escaping and output formats before adding the commands to README examples.

**[Note]** The DeviceClass name `ai-inference-large` above is from the original spec draft. The offering names settled for this project are `gpu-inference`, `low-latency-network`, and `gpu-nic-aligned` (see `docs/specs/001-intent-based-offerings.md`); use those in any documentation that ships.

### FR-12: Error Handling

**[AMENDED — resolved into FR-2 above]**

> Define whether the controller should withhold the affected DeviceClass update or omit the summary with a clearly surfaced error. Prefer fail-safe behavior and follow existing controller conventions.

Resolved: **omit the summary annotations with a surfaced error, do not withhold the DeviceClass update.** See the amended FR-2 for the full rationale (matches the existing `publishErrs` accumulation convention; withholding risks a silently stale summary).

---

## 7. Implementation Structure

```text
internal/controller/
    capability_summary.go
    capability_summary_test.go
```

### Core generation

```go
func BuildCapabilitySummary(
    config PartitionConfig,
) (CapabilitySummary, error)
```

### Text formatting

```go
func FormatCapabilitySummary(
    summary CapabilitySummary,
) string
```

### Serialization

Implement deterministic JSON serialization using Go's `encoding/json` and the shared canonicalizer from FR-9. Normalization means canonical collection ordering and compact serialization; preserve capacity quantity strings verbatim.

### Integration

**[AMENDED — placement resolved]**

Call the summary generator in the existing DeviceClass generation path once the final PartitionConfig has been determined. Avoid reparsing the original grouping ConfigMap. The existing DeviceClass manager should remain responsible for publishing and reconciling DeviceClasses.

**Placement: annotate every TC-managed DeviceClass this feature can reach — per-instance and aggregate alike — not aggregate-only.** `buildGroupingDeviceClass` is called for per-instance classes (`-rail<N>` names) and the aggregate class; `buildDeviceClassFromCache` is called for per-instance, partition-type-aggregate, and tier classes. Per-instance configs describe one instance. Aggregate summaries may be generated only after the FR-5 capacity-intersection invariant is enforced; otherwise omit the summary and surface an error. Intent metadata remains aggregate-only because it is administrator-authored presentation metadata. Call `BuildCapabilitySummary` and annotate each produced `*resourcev1.DeviceClass` in both construction paths.

---

## 8. Tests

### Unit tests

The original spec called for 22 unit test cases. Enumerated:

1. Single device class.
2. Multiple device classes.
3. Consumable CPU capacity.
4. Consumable memory capacity.
5. Multiple capacity keys.
6. Multiple GPU requests.
7. GPU/NIC grouping.
8. NUMA alignment.
9. PCIe-root alignment.
10. Multiple topology constraints.
11. Required and preferred enforcement.
12. Unknown vendor-specific device classes.
13. Unknown capacity keys.
14. Partition mode.
15. Common Expression Language (CEL) selectors.
16. Deterministic output.
17. Invalid quantities return a construction error; invalid aggregate inputs prevent aggregate publication.
18. Empty configuration.
19. Inconsistent request identifiers.
20. Oversized output.
21. Preservation of DeviceClass metadata.
22. Heterogeneous or aggregate partition classes.

### Integration tests

Verify:

- Generated DeviceClasses include capability metadata.
- Summary data matches the associated PartitionConfig.
- Updates modify the summary appropriately.
- Metadata remains stable across repeated reconciliation.
- Existing grouping and partition generation is unchanged.
- Existing ResourceClaims continue to expand correctly.

### Property-based or fuzz tests

Where practical, verify:

- Transformation never mutates PartitionConfig.
- Identical semantic inputs generate identical outputs.
- JSON output remains valid.
- Arbitrary capacity keys and class names do not cause panics.
- Malformed configurations fail safely.

### Added by review

Beyond the original inventory, add tests that cover:

- An aggregate whose alternatives have differing valid capacities summarizes the intersected minimum.
- A capacity key absent from the first, middle, or final alternative remains absent from the aggregate regardless of input order; cover at least three alternatives so a later input cannot reintroduce a key after the intersection becomes empty.
- An aggregate with any unparseable contributing quantity returns an error and is not published.
- For valid quantities, per-instance summaries preserve the configured capacity map unchanged.
- Determinism when aggregate input order changes and capacities differ.
- Summary `Kind` copies `PartitionConfig.Kind`; the grouping, partition, and tier DeviceClass names do not affect it.
- Distinct alignments with the same attribute and requests but different enforcement serialize deterministically; sub-resources tied on device class and count use the remaining serialized fields as tie-breakers.
- A merged annotation map near the 256 KiB limit, including foreign annotations; verify the summary is omitted and the rest of the DeviceClass update proceeds.
- A conflict retry where a concurrent foreign annotation pushes the latest merged map over the limit; verify validation and drop-order handling run again before the retry update.
- Oversized summaries and invalid per-instance quantities; verify stale summary keys are removed while the DeviceClass configuration is still reconciled.

**Added:** a unit test exercising `BuildCapabilitySummary` against a `PartitionConfig` derived from an asymmetric grouping aggregate after the FR-5 reducer correction. Assert that it reflects the guaranteed driver/count subset and minimum capacity. Keep this test alongside the reducer tests so the end-to-end summary contract cannot pass while the source aggregate is wrong.

---

## 9. Documentation

Update the README with a section titled:

**Hardware Capability Discovery**

Explain:

1. Why capability summaries exist.
2. How they differ from administrator-defined intent metadata.
3. How summaries are derived from PartitionConfig.
4. How to inspect summaries using kubectl.
5. How to use the JSON output in a catalog or UI.
6. How consumable DRA capacity is represented.
7. How topology guarantees are described.
8. How aggregate DeviceClasses and variants are handled.
9. Why capability summaries do not imply current availability.

Include an example that shows the complete progression:

```text
Administrator grouping ConfigMap
        ↓
TC-generated PartitionConfig
        ↓
Derived CapabilitySummary
        ↓
Published DeviceClass
        ↓
User discovery through Kubernetes API
        ↓
Standard DRA ResourceClaim
```

---

## 10. Non-Goals

Do not implement:

- Another hardware-definition schema.
- A new HardwareOffering CRD.
- A second controller.
- A catalog web application.
- Resource availability monitoring.
- Automatic workload hardware sizing.
- GPU or CPU performance benchmarking.
- Hardware recommendation logic.
- Device-plugin integration.
- KubeVirt-specific APIs.
- VEP-300 or VEP-470 integration.
- Changes to the DRA allocation scheduler.
- Changes to ResourceClaim expansion algorithms.

This feature is metadata generation and discovery only.

---

## 11. Acceptance Criteria

- [ ] CapabilitySummary is derived entirely from the final PartitionConfig.
- [ ] No hardware quantities need to be duplicated in administrator metadata.
- [ ] Summaries support arbitrary DRA resources.
- [ ] Consumable capacity semantics are preserved.
- [ ] Topology constraints retain their enforcement semantics.
- [ ] Existing grouping and partition classes are supported.
- [ ] Heterogeneous classes do not publish false uniform guarantees.
- [ ] Summaries are available through DeviceClass annotations.
- [ ] Human-readable summaries are generated automatically.
- [ ] Output is deterministic and versioned.
- [ ] Reconciliation correctly handles updates and deletions.
- [ ] Existing ResourceClaim expansion behavior is unchanged.
- [ ] Existing configurations remain compatible.
- [ ] Unit and integration tests pass.
- [ ] Documentation includes working discovery examples.
- [ ] No additional controller or CRD is introduced.

**[AMENDED]** This criterion is not met until the aggregate-capacity gaps in Readiness blocker 1 are fixed and covered by the tests in §8. Once fixed, heterogeneous summaries publish only fields guaranteed across every alternative; no uncertainty field is needed.

"Existing ResourceClaim expansion behavior is unchanged" needs one qualification: the capacity intersection fix *did* change the capacity values in aggregate DeviceClasses' `Spec.Config`, which claim expansion consumes. That change is a bug fix landed separately from this feature (aggregates previously advertised a nondeterministic, sometimes-unsatisfiable capacity); this feature itself introduces no further change to expansion.

---

## 12. Instructions for Claude Code / implementing agent

**[AMENDED]** PR #13 and PR #14 are merged to `main`; verify both are present on the implementation base. PR #14 does not fully satisfy the capacity guarantee: implement the additional reducer corrections in "Code fix 2" and Readiness blocker 1 before capability summaries trust aggregate configs. Implement within the existing controller and DeviceClass publishing paths; follow the validation and delivery requirements in this specification.

---

## 13. Future Extensions

This capability summary becomes the foundation for a richer hardware offering catalog.

Possible future features include:

- Structured filtering by verified GPU, CPU, memory or networking capabilities.
- Cluster-wide discovery of hardware offerings.
- Hardware-offering comparison.
- Optional available-capacity reporting.
- Recommendations based on workload requirements.
- Integration with KubeVirt DeviceProvider offerings.
- Integration with other Kubernetes workload operators and platforms.

These enhancements must consume the generated capability summary rather than creating another source of hardware configuration.

**Long-term architectural principle: The administrator defines hardware composition once; TC derives the technical capabilities; users discover and select the level of abstraction appropriate to their needs.**
