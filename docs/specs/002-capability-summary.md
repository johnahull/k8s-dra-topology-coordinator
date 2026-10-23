# Implementation Specification: Automatically Generated Hardware Capability Summaries

**Project:** `rh-ecosystem-edge/k8s-dra-topology-coordinator`
**Feature:** Derived Hardware Capability Summaries
**Status:** Proposed — review amendments recorded; one open item remains (annotation budget, blocker 2 below)
**Prerequisite:** Intent-Based Hardware Offerings metadata enhancement (PR2, #13, not yet merged to `main`); **and** both aggregate-intersection fixes — count and capacity — which ship together in PR #14 (https://github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/pull/14, not yet merged). This feature cannot be safely implemented without both.
**Initial scope:** Kubernetes DRA DeviceClasses
**Priority:** High

---

## Amendment note (read this first)

This spec has been through three review passes against the actual code: a manual review, an architect review (`feature-dev:code-architect`) that verified every claim with file:line citations, and a follow-up review of the amendments themselves. Each pass found a real defect in the code the feature depends on, and both code defects have now been fixed. Amendments are marked `[AMENDED]` inline.

### Readiness blockers

1. ~~Aggregate capacity values are copied from one qualifying instance without comparison.~~ **Resolved in code** — see "Resolved blocker 2" below. Both counts and capacity are now intersected at the source, so an aggregate's `PartitionConfig` is a genuine guarantee and `BuildCapabilitySummary` can trust it without a provenance parameter or an omit-on-divergence policy.
2. **OPEN.** The 32 KiB summary-pair limit does not ensure the final merged annotation map stays below Kubernetes' 256 KiB combined annotation limit. The check must account for existing foreign and TC-owned annotations at publish time. See amended FR-2 — the requirement is clear but the *placement* is not yet settled, and settling it is a prerequisite for implementation.
3. ~~The `unchanged` placeholder sections and the 22-case test inventory are missing.~~ **Resolved** — every placeholder section has been restored verbatim from the original spec draft (§5, FR-4, FR-6, FR-8, FR-10, FR-11, §9, §10, §11, §13). The one genuine remaining gap is the 22-case unit-test inventory in §8, which the original spec listed only as a count, not as enumerated cases; §8 now enumerates concrete cases in its place.

### Resolved blocker 1: grouping aggregates did not intersect device counts

The original §6 FR-5 assumed "the final PartitionConfig associated with each generated class" was always a trustworthy, guaranteed-across-every-realization description for aggregate classes. It was not, for one specific aggregate type.

Verified in `internal/controller/deviceclass_manager.go`: `SyncDeviceClasses`'s partition-type aggregate (the pcieroot/numa aggregate, and by inheritance the tier-named aggregates) intersects driver counts — a driver's count only survives if it's present on *every* partition of that type (`driverSeen[driver] == total`), at the minimum count across instances, with a System Locality Information Table (SLIT) distance-based reachability carve-out for NUMA-shareable drivers. This establishes a count guarantee; it does not establish that capacity values are identical or conservative across those partitions.

`SyncGroupingDeviceClasses`'s grouping aggregate (the one named after the grouping itself, e.g. `gpu-nic-pair`) did **not** do this. It picked one arbitrary instance — deterministically the lowest `RailIndex`, as of a prior correctness fix — and published that single instance's `PartitionConfig` as the entire aggregate's config. If the grouping is asymmetric across rails (devices present on some rails but not others — explicitly the reason groupings exist, per the code's own comment on the partition-aggregate intersection: "Asymmetric devices... handled via grouping DeviceClasses instead"), the grouping aggregate's capability summary would describe one rail's hardware as if it were guaranteed for the whole grouping. A pure `BuildCapabilitySummary(config PartitionConfig) (CapabilitySummary, error)` has no way to detect or correct this from inside the function — `PartitionConfig` carries no provenance about whether it was intersected or arbitrarily picked, so FR-5's "if PartitionConfig does not contain enough information to distinguish alternatives, report that limitation" could not have been implemented against the original code no matter how carefully `BuildCapabilitySummary` was written.

**Fix (PR #14, branch `fix/grouping-aggregate-intersection`):** `SyncGroupingDeviceClasses` now intersects driver presence and counts across instances of a grouping name, retaining the minimum count. The NUMA-SLIT-reachability carve-out was deliberately **not** ported — it's a partition-type-specific optimization tied to NUMA distance semantics with no equivalent in grouping alignment (pcieRoot/numaNode/socket are a matching key here, not a distance model). Regression test: `TestDeviceClassManager_SyncGroupingDeviceClasses_AggregateIntersectsAsymmetricRails`.

### Resolved blocker 2: aggregate capacity was nondeterministic and unsafe

The count fix above left capacity untouched, and a follow-up review caught that this was not merely incomplete but actively broken — in both aggregate paths, including the partition-type path that predates all of this work.

Both accumulators assigned capacity with an unconditional overwrite (`existing.minCap[driver] = cap`), despite the field name. Last writer won, and the writer was chosen by Go map iteration order over `seen`. Reproduced before fixing: two PCIe-root partitions under one aggregate key with capacities `16` and `8`, synced 40 times, produced `16` on 5 runs and `8` on 35.

This is not a contrived input. `partition_builder.go` derives per-partition capacity as `divideQuantity(capVal, numRoots)` where `numRoots` is the PCIe-root count of that partition's NUMA node, so any node whose NUMA nodes have different root counts produces exactly this shape.

Two consequences, both real and both predating this feature:

- **Nondeterminism.** The aggregate's `Spec.Config` bytes changed between reconciles, so `publishDeviceClass`'s no-op short-circuit never fired for that class and it issued an Update every reconcile — defeating the idempotency work in PR #12.
- **Incorrectness.** Advertising the larger capacity means claims expanded against the aggregate request more than the smaller partition can supply, and fail to schedule there.

**Fix (PR #14, same branch as the count fix):** capacity now reduces to the per-key minimum over the keys shared by every alternative, via `mergeMinCapacity`. Keys absent from any alternative are dropped rather than advertised as guaranteed. Quantities are compared with `resource.Quantity.Cmp`; values that fail to parse fall back to a stable lexical choice, arbitrary but not iteration-order-dependent.

**Consequence for this spec — this supersedes the earlier "omit the summary when capacities differ" policy.** That policy was the right call against the code as it stood, where the config could not be trusted. Now that both counts and capacity are intersected at the source, an aggregate's `PartitionConfig` *is* a guarantee in both dimensions, and the summary can reflect it faithfully with no special handling:

- `BuildCapabilitySummary(config PartitionConfig)` stays a pure function with no provenance or confidence parameter.
- No omit-on-divergence policy, no uncertainty field in the published schema.
- Per-instance summaries copy their configured capacity unchanged, as before.

On the specific question of *minimum* versus *exact equality*: minimum is correct and is what the code does. The two dimensions are now consistent — counts already took the minimum, and taking the minimum of a capacity is the same kind of claim ("guaranteed on every alternative"). Exact-equality-or-omit would be strictly more conservative but would discard a true, useful guarantee: if one partition supplies 16 and another 8, "8" is genuinely guaranteed everywhere and is more informative than publishing nothing. This does not conflict with FR-8's caution about quantity normalization — see the amendment at the end of FR-8.

### Other amendments (smaller, non-blocking)

- **FR-2 / FR-12 annotation size — concrete bound added.** See amended FR-2 below.
- **Placement resolved: annotate both aggregate and per-instance DeviceClasses**, not aggregate-only. See amended §7 Integration below.
- **`TopologyCapability.Enforcement` should stay `EnforcementMode`, not `string`** — it's already a named type in this package (`internal/controller/topology_rules.go`) that marshals to a JSON string fine; widening it to `string` in the summary loses exhaustive-switch safety in `FormatCapabilitySummary` for no benefit. See amended §4 below.
- **FR-9 determinism — reuse existing sort logic, don't reimplement.** See amended FR-9 below.

---

## 1. Objective

*(unchanged from original)*

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

This information should be used to generate a normalized capability summary published on the corresponding DeviceClass.

**Primary design principle: The existing PartitionConfig remains the source of truth. The capability summary is a derived, read-only representation of the hardware requirements.**

The capability summary must never independently control resource allocation.

---

## 2. Goals

*(unchanged)*

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

*(unchanged — confirmed verbatim against `internal/controller/deviceclass_manager.go`)*

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

**[AMENDED]** As of the intersection fix, this requirement is satisfiable for every DeviceClass TC currently generates: `PartitionConfig` is fully resolved — no live Kubernetes queries, no ConfigMap re-reads — by the time it reaches `buildGroupingDeviceClass`/`buildDeviceClassFromCache`, the two call sites where this feature will integrate. Confirmed: a pure `BuildCapabilitySummary(config PartitionConfig)` dropped into either call path has no access to anything outside its argument by construction.

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

*(unchanged)*

- `Resources` must describe the underlying DRA requests, not an assumed allocation result.
- `DeviceClass` must retain its full name.
- `Count` must preserve the actual request count semantics.
- `Capacity` must preserve the original qualified capacity names and values.
- `PartitionMode` must retain provider-specific partition information.
- `Selectors` must copy every selector from the corresponding `SubResourceConfig`, preserving the expression text and order.
- `Topology` must preserve the actual alignment constraints.
- `SchemaVersion` must allow future schema evolution.

Do not infer a device type from its class name alone.

For example, a DeviceClass containing the word `gpu` does not guarantee the device is a GPU.

The initial summary should remain generic, even when the devices happen to be CPUs, GPUs, NICs, or memory.

### Important distinction

*(unchanged)*

A `count` of `1` combined with a consumable CPU capacity of `16` does not mean the workload requests one CPU.

It may represent one grouped DRA device with a consumable capacity request for 16 CPUs.

Preserve both fields — do not collapse these into an inaccurate resource count.

---

## 5. Example Derived Summary

Suppose the resolved PartitionConfig contains:

```json
{
  "kind": "grouping",
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

The capability generator should create an equivalent normalized representation:

```json
{
  "schemaVersion": "v1alpha1",
  "kind": "grouping",
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

*(unchanged)*

```go
func BuildCapabilitySummary(
    config PartitionConfig,
) (CapabilitySummary, error)
```

**[AMENDED — confirms original intent, now actually buildable]** Because every `PartitionConfig` reaching this function is now trustworthy (intersected where it needs to be, concrete and non-heterogeneous everywhere else), this signature stays exactly as proposed — a pure function with no provenance/confidence parameter. This was flagged during architect review as a real risk if the aggregate fix hadn't landed first; it no longer is.

The implementation must:

- Read existing sub-resource definitions.
- Preserve all device counts.
- Preserve consumable capacity values.
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
        "kind": "grouping",
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
- **Before each API create or update, check the final merged annotation map against Kubernetes' 256 KiB combined limit.** Count `len(key) + len(value)` for every annotation, including existing foreign annotations and all TC-owned annotations. `publishDeviceClass` is the first point where the final merged map is available. If adding the summary would exceed the limit, remove both summary keys from the desired annotations, log a surfaced error naming the DeviceClass, and publish the remaining metadata and `Spec.Config`.

  **[OPEN — readiness blocker 2; settle before implementing.]** The *requirement* above is correct: a fixed 32 KiB feature budget cannot by itself guarantee the combined limit is respected, since foreign annotations are unbounded and unknown until publish time. The *placement* is not settled, and the naive reading has two problems worth resolving first:

  1. `publishDeviceClass` is generic infrastructure shared by every DeviceClass TC publishes. Teaching it to recognize and drop two specific capability-summary keys couples a shared function to one feature.
  2. PR2's four intent annotations (`display-name`, `description`, `categories`, `workload-types`) have identical exposure to the same 256 KiB ceiling, with no equivalent carve-out. On overflow, why are capability keys sacrificed and intent keys preserved? That precedence is a real decision and is currently unstated.

  A cleaner shape, if it holds up: a generic TC-owned annotation budget enforced in `publishDeviceClass` with a documented, feature-agnostic drop order, rather than feature-specific key removal. Whoever implements this should pick an approach and record the reasoning here before writing the check.
- **On oversized or unserializable summaries: omit the summary annotations and log a surfaced error naming the DeviceClass, not withhold the entire DeviceClass update.** This matches the codebase's existing convention — `SyncDeviceClasses`/`SyncGroupingDeviceClasses` already accumulate per-object errors (`publishErrs`) and continue processing other objects rather than failing the whole sync on one bad object. Omitting the two annotation keys (and deleting them if previously present, via the existing `mergeTCOwnedMetadata` prefix-based removal) while still publishing the rest of the DeviceClass (labels, intent metadata if present, `Spec.Config`) is self-correcting and loses only this one feature's output for that one object, with a clear log line as evidence.

### FR-3: Generate Human-Readable Summaries

*(unchanged)*

In addition to the machine-readable JSON, generate a concise human-readable description via the `nodepartition.dra.k8s.io/capability-summary` annotation.

Requirements:

- Generate the text from the normalized capability summary.
- Do not read independent administrator-entered hardware descriptions.
- Use stable resource ordering.
- Keep text within a bounded length.
- Do not assume driver resource names imply specific physical hardware.
- Use accurate DRA terminology when capacity and device count differ.

**[AMENDED — implementation note, not a requirement change]** The example text ("CPU capacity dra.cpu/cpu=16 (1 grouped device), 2 × gpu.example.com...") correctly keeps capacity and count separate per FR-8. When implementing `FormatCapabilitySummary`, generate this phrasing from a per-resource template that always surfaces both fields whenever `Capacity` is non-empty — not a CPU-specific special case. A hardcoded "if this looks like CPU, phrase it this way" branch inside the formatter would quietly violate FR-6 (no hardcoded CPU/GPU/NIC enum) even though the Go types themselves stayed generic.

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
    nodepartition.dra.k8s.io/capability-summary: "2 × gpu.example.com; CPU capacity dra.cpu/cpu=16"
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

Original defensive language (retain as a durable invariant; it is now satisfied at the source for both counts and capacity):

> If a class represents alternative physical realizations with different underlying configurations, do not publish one representative configuration as though it is guaranteed for every realization. For heterogeneous classes, either (1) publish explicitly represented configuration variants, with their applicability, or (2) publish only characteristics guaranteed across every alternative and clearly mark unresolved details. If the existing PartitionConfig does not contain enough information to distinguish alternatives, report that limitation rather than inferring uniformity.

**Status as of this amendment:** satisfied by option (2) — "publish only characteristics guaranteed across every alternative" — in both dimensions, for every aggregate type currently generated:

- Partition-type aggregates (pcieroot/numa) and tier-named aggregates: intersect driver presence and counts, with an SLIT-reachability carve-out for NUMA-shareable drivers; capacity reduces to the per-key minimum over keys shared by every partition.
- Grouping aggregates: the same, minus the NUMA-SLIT carve-out (no grouping-alignment equivalent).

Because this is enforced where the `PartitionConfig` is built, `BuildCapabilitySummary` does not need to re-derive or re-verify "is this guaranteed" — it can trust its input, for every call site. **Do not add a `Representative`/`Guaranteed` boolean field to `CapabilitySummary`** — this was considered twice during review and rejected both times: it would be the only place in the DeviceClass surface where "guaranteed across all instances" doesn't hold for an aggregate, inconsistent with how every aggregate now behaves, and it would let future heterogeneous-aggregate cases quietly opt out of the real fix instead of being corrected at the source.

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
- Use Kubernetes quantity parsing only when normalization is semantically safe.
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

**[AMENDED]** "Use Kubernetes quantity parsing only when normalization is semantically safe" is satisfied, not violated, by the aggregate minimum described under FR-5. The aggregation path compares quantities with `resource.Quantity.Cmp` to select the smallest value among alternatives — it does not sum, scale, or unit-convert them for display, and the value published is always one of the input values verbatim. Selecting a minimum is the one reduction that is formally valid here, because the smallest value is satisfiable wherever any of the alternatives is.

### FR-9: Deterministic Output

**[AMENDED]**

Identical PartitionConfigs must generate identical summaries regardless of map iteration order.

Requirements:

- Sort resources using a stable, documented ordering where ordering is not semantically meaningful.
- Sort capacity-map keys.
- Treat `AlignmentConfig.Requests` as a set of request identifiers and sort them lexically; retain request identity.
- Preserve ordering for every other list unless its semantics are explicitly documented as order-independent.
- Serialize consistently.
- Do not include timestamps or nondeterministic values.

Avoid unnecessary DeviceClass updates when nothing relevant has changed.

**Added:** `internal/controller/deviceclass_manager.go` already has a `sortPartitionConfig` helper (sorts `SubResources` by `(DeviceClass, Count)`, sorts each `Alignments[i].Requests`, then sorts `Alignments` by `(Attribute, joined Requests)`) that runs on every `PartitionConfig` before it's marshaled into the DeviceClass's opaque config. By the time `BuildCapabilitySummary` receives its input, that input is already canonically ordered. **Do not have `BuildCapabilitySummary` define a second, independent sort order for the same logical fields** (`DeviceClass`/`Count`/`Attribute`/`Requests` map directly onto `ResourceCapability`/`TopologyCapability`) — two independently-hand-maintained orderings for the same data is exactly the kind of drift this requirement exists to prevent, and would be easy to let drift apart silently across future edits to either function. Factor the sort-key comparator out so both `sortPartitionConfig` and the capability-summary path call the same logic, or have `BuildCapabilitySummary` explicitly document that it trusts `sortPartitionConfig`'s invariant and re-sort its own output using literally the same key functions (not reimplemented ones).

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

*(unchanged)*

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

Implement deterministic JSON serialization using the normal Go encoding/json facilities and appropriately normalized data (see FR-9 amendment — reuse `sortPartitionConfig`'s sort keys rather than reimplementing).

### Integration

**[AMENDED — placement resolved]**

Call the summary generator in the existing DeviceClass generation path once the final PartitionConfig has been determined. Avoid reparsing the original grouping ConfigMap. The existing DeviceClass manager should remain responsible for publishing and reconciling DeviceClasses.

**Placement: annotate every TC-managed DeviceClass this feature can reach — per-instance and aggregate alike — not aggregate-only.** `buildGroupingDeviceClass` is called identically for per-instance classes (`-rail<N>` names) and the aggregate class; `buildDeviceClassFromCache` is called identically for per-instance, partition-type-aggregate, and tier classes. Per-instance configs are homogeneous by construction — one `GroupingInstance`/`PartitionDevice` each — so FR-5's alternative-realization concern does not arise there. Aggregates need no special handling either, now that counts and capacity are both intersected at the source: every call site hands `BuildCapabilitySummary` a config that is already a guarantee. This is a different placement decision than PR2's intent metadata (which is aggregate-only, a *presentation* choice to keep `kubectl get deviceclasses` output to one row per offering) — the capability summary is a per-object derived fact about that object's own resolved configuration, not administrator-authored prose, so there's no discovery-noise argument for omitting it from per-instance classes. Call `BuildCapabilitySummary` + annotate inside (or immediately around) both `buildGroupingDeviceClass` and `buildDeviceClassFromCache`, once per `*resourcev1.DeviceClass` object produced, regardless of which emission loop produced it.

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
15. CEL selectors.
16. Deterministic output.
17. Invalid quantities.
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

- An aggregate whose alternatives have differing capacity summarizes the intersected minimum, and a capacity key absent from any alternative does not appear in the summary at all. (The intersection itself is covered at the source by `TestDeviceClassManager_AggregateCapacityIsMinimumAndDeterministic` and `TestMergeMinCapacity`; this case asserts the summary faithfully reflects it.)
- Per-instance summaries preserve the configured capacity map unchanged.
- Determinism when aggregate input order changes and capacities differ.
- A merged annotation map near the 256 KiB limit, including foreign annotations; verify the summary is omitted and the rest of the DeviceClass update proceeds.
- Oversized and unserializable summaries; verify stale summary keys are removed while the DeviceClass configuration is still reconciled.

**Added:** a unit test exercising `BuildCapabilitySummary` against a `PartitionConfig` derived from an intersected, asymmetric grouping aggregate (i.e., feed it the *output* of the now-fixed `SyncGroupingDeviceClasses` aggregate path for a grouping where rails differ), asserting the summary reflects the intersected driver/count subset and the intersected minimum capacity. This tests the intersection end-to-end, not just at the `SyncGroupingDeviceClasses` layer (which has its own regression tests, `TestDeviceClassManager_SyncGroupingDeviceClasses_AggregateIntersectsAsymmetricRails` and `TestDeviceClassManager_AggregateCapacityIsMinimumAndDeterministic`, added alongside the two fixes).

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

**[AMENDED]** "Heterogeneous classes do not publish false uniform guarantees" is achievable as stated: both device counts and capacity are now intersected at the source for every aggregate type (see FR-5). It does not require a weakened implementation or an uncertainty field in the schema.

"Existing ResourceClaim expansion behavior is unchanged" needs one qualification: the capacity intersection fix *did* change the capacity values in aggregate DeviceClasses' `Spec.Config`, which claim expansion consumes. That change is a bug fix landed separately from this feature (aggregates previously advertised a nondeterministic, sometimes-unsatisfiable capacity); this feature itself introduces no further change to expansion.

---

## 12. Instructions for Claude Code / implementing agent

**[AMENDED]** §12's original "Second: Review design decisions" step asked the implementer to investigate whether aggregate DeviceClasses have a single authoritative `PartitionConfig`, and to document any cases requiring variants or conservative summaries. Both halves of that investigation are now done and acted on: the count intersection and the capacity intersection, both in PR #14. Verify both are present on your implementation base before assuming FR-5 is satisfied — if either is missing, you will reproduce the exact problems documented at the top of this file. The one item still genuinely open is the annotation budget (readiness blocker 2); settle its placement before publishing summaries.

Everything else in the original §12 (inspect first, implement, validate with `make test`/`make lint`/`make build`, report unavailable tests rather than claiming they passed, deliver the standard summary) is unchanged.

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
