# Implementation Specification: Automatically Generated Hardware Capability Summaries

**Project:** `rh-ecosystem-edge/k8s-dra-topology-coordinator`
**Feature:** Derived Hardware Capability Summaries
**Status:** Proposed — amended after spec review and architect review against the actual codebase
**Prerequisite:** Intent-Based Hardware Offerings metadata enhancement (PR2, #13, not yet merged to `main` as of this amendment); **and** `fix/grouping-aggregate-intersection` (committed, see "Resolved blocker" below) — this feature cannot be safely implemented without it.
**Initial scope:** Kubernetes DRA DeviceClasses
**Priority:** High

---

## Amendment note (read this first)

This spec was reviewed in two passes — a manual review against the current code, then an architect review (`feature-dev:code-architect`) that verified every claim with file:line citations. One finding was a **go/no-go blocker**, now resolved by a code fix. Everything else is a clarification or a concrete number filled in where the original spec left a principle. The amendments are folded directly into the sections below, marked `[AMENDED]`; the rest of the original spec is unchanged.

### Resolved blocker: grouping aggregates did not intersect

The original §6 FR-5 assumed "the final PartitionConfig associated with each generated class" was always a trustworthy, guaranteed-across-every-realization description for aggregate classes. It was not, for one specific aggregate type.

Verified in `internal/controller/deviceclass_manager.go`: `SyncDeviceClasses`'s partition-type aggregate (the pcieroot/numa aggregate, and by inheritance the tier-named aggregates) already compute a true intersection — a driver's count only survives into the aggregate if it's present on *every* partition of that type (`driverSeen[driver] == total`), taking the minimum count across instances, with an SLIT-reachability carve-out for NUMA-shareable drivers. This satisfies FR-5 option 2 ("publish only characteristics guaranteed across every alternative") for free — it was already built, years before this spec existed, to solve the exact asymmetric-hardware problem FR-5 describes.

`SyncGroupingDeviceClasses`'s grouping aggregate (the one named after the grouping itself, e.g. `gpu-nic-pair`) did **not** do this. It picked one arbitrary instance — deterministically the lowest `RailIndex`, as of a prior correctness fix — and published that single instance's `PartitionConfig` as the entire aggregate's config. If the grouping is asymmetric across rails (devices present on some rails but not others — explicitly the reason groupings exist, per the code's own comment on the partition-aggregate intersection: "Asymmetric devices... handled via grouping DeviceClasses instead"), the grouping aggregate's capability summary would describe one rail's hardware as if it were guaranteed for the whole grouping. A pure `BuildCapabilitySummary(config PartitionConfig) (CapabilitySummary, error)` has no way to detect or correct this from inside the function — `PartitionConfig` carries no provenance about whether it was intersected or arbitrarily picked, so FR-5's "if PartitionConfig does not contain enough information to distinguish alternatives, report that limitation" could not have been implemented against the original code no matter how carefully `BuildCapabilitySummary` was written.

**Fix (committed on branch `fix/grouping-aggregate-intersection`):** `SyncGroupingDeviceClasses`'s aggregate-building loop now mirrors the partition-aggregate pattern exactly — a `groupingAggregateState` accumulator tracks `driverSeen`/`minCounts`/`minCap` across all instances of a grouping name, and only drivers present on every instance survive into the aggregate, at the minimum count. The NUMA-SLIT-reachability carve-out was deliberately **not** ported — it's a partition-type-specific optimization tied to NUMA distance semantics with no equivalent in grouping alignment (pcieRoot/numaNode/socket are a matching key here, not a distance model); a straight intersection is the correct, conservative behavior. All 172 existing tests pass unchanged (symmetric groupings intersect to the same result as the old arbitrary pick), plus a new regression test for the asymmetric case, confirmed to fail against the prior code.

**Consequence for this spec:** FR-5 is now satisfiable as originally written, for every aggregate type the codebase currently produces (partition-type, tier, and grouping aggregates all intersect). `BuildCapabilitySummary` can now be a true pure function over `PartitionConfig` with no provenance parameter — the input is always trustworthy by the time it reaches the function, for every call site. If a future aggregate type is added that does *not* intersect, FR-5's defensive requirement (report the limitation rather than inferring uniformity) remains the correct fallback — it's just not needed for anything that exists today.

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
- `Selectors` must preserve any selectors needed to understand request characteristics.
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

*(unchanged — the example is illustrative and doesn't depend on the aggregate-intersection fix; it describes a single concrete grouping, not an aggregate spanning heterogeneous instances)*

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
- Preserve relevant selectors.
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
- **Check serialized size before calling `publishDeviceClass`, not after.** There is no per-annotation-key limit in the Kubernetes API — only a combined 256 KiB cap across *all* annotations on the object. `publishDeviceClass`'s existing merge-preserving logic (`internal/controller/deviceclass_manager.go`) means foreign annotations on the same object, and — once PR2/#13 merges — up to four more TC-owned annotations (`displayName`/`description`/`categories`/`workload-types`) on the same aggregate, all count against that same combined limit. `publishDeviceClass` has no size-checking logic anywhere today; this feature is entirely responsible for its own budget. Concrete bound: **reject (see below) if the `capabilities` + `capability-summary` annotation pair alone would exceed 32 KiB**, leaving generous headroom under the 256 KiB combined ceiling for PR2's annotations plus whatever foreign annotations other tooling adds. Measure with `len()` on the final UTF-8-encoded annotation values before they're placed into `dc.Annotations`, not after a failed API call.
- **On oversized or unserializable summaries: omit the summary annotations and log a surfaced error naming the DeviceClass, not withhold the entire DeviceClass update.** This matches the codebase's existing convention — `SyncDeviceClasses`/`SyncGroupingDeviceClasses` already accumulate per-object errors (`publishErrs`) and continue processing other objects rather than failing the whole sync on one bad object. Withholding the DeviceClass update entirely would leave a stale summary from a prior reconcile in place — silently wrong if the underlying partition shape changed since then, which is worse than omitting. Omitting the two annotation keys (and deleting them if previously present, via the existing `mergeTCOwnedMetadata` prefix-based removal) while still publishing the rest of the DeviceClass (labels, intent metadata if present, `Spec.Config`) is self-correcting and loses only this one feature's output for that one object, with a clear log line as evidence.

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

*(unchanged)*

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

Original defensive language (still correct as a durable invariant, now satisfied by construction rather than needing a runtime check):

> If a class represents alternative physical realizations with different underlying configurations, do not publish one representative configuration as though it is guaranteed for every realization. For heterogeneous classes, either (1) publish explicitly represented configuration variants, with their applicability, or (2) publish only characteristics guaranteed across every alternative and clearly mark unresolved details. If the existing PartitionConfig does not contain enough information to distinguish alternatives, report that limitation rather than inferring uniformity.

**Status as of this amendment:** satisfied by option (2), for every aggregate type currently generated:

- Partition-type aggregates (pcieroot/numa) and tier-named aggregates: already computed a true intersection before this spec was written (`internal/controller/deviceclass_manager.go`, the `aggStates`/`driverSeen`/`minCounts`/`minCap` pattern, with an SLIT-reachability carve-out for NUMA-shareable drivers). No change needed.
- Grouping aggregates: fixed by `fix/grouping-aggregate-intersection` to use the identical pattern (minus the NUMA-SLIT carve-out, which has no grouping-alignment equivalent). A driver only appears in the aggregate's `PartitionConfig` if present on every instance of that grouping name, at the minimum count across instances.

Because this is now enforced at the source (where the `PartitionConfig` is built), `BuildCapabilitySummary` does not need to re-derive or re-verify "is this guaranteed" — it can trust its input. **Do not add a `Representative`/`Guaranteed` boolean field to `CapabilitySummary` as an alternative fix** — this was considered during architect review and rejected: it would be the only place in the DeviceClass surface where "guaranteed across all instances" doesn't hold for an aggregate class, inconsistent with how every other aggregate already behaves, and would let future heterogeneous-aggregate cases quietly opt out of the real fix instead of being corrected at the source. If a future aggregate type is added to the codebase that cannot be made to intersect, the correct response is the same as it was before this amendment: block capability-summary annotation on that specific aggregate type until it can be fixed, not add a permanent escape hatch to the schema.

### FR-6: Preserve Arbitrary DRA Resources

*(unchanged)*

### FR-7: Topology Summary

*(unchanged)*

Preserve topology requirements from `AlignmentConfig`: full qualified attribute name, participating generated request identifiers, required versus preferred enforcement, multiple simultaneous alignment constraints.

Do not infer PCIe-root or NUMA guarantees solely from a grouping's name. Do not turn a preferred constraint into a guaranteed capability. Do not describe fallback behavior as guaranteed unless the actual expanded configuration enforces it.

**[Confirmed, no change needed]** `EnforcementMode` already has exactly two values, `"required"` and `"preferred"` (`internal/controller/topology_rules.go`), matching this requirement's vocabulary directly — `BuildCapabilitySummary` just copies the field through (see §4 amendment: keep it typed, not stringified).

### FR-8: Capacity Semantics

*(unchanged)*

### FR-9: Deterministic Output

**[AMENDED]**

Identical PartitionConfigs must generate identical summaries regardless of map iteration order.

Requirements:

- Sort resources using a stable, documented ordering where ordering is not semantically meaningful.
- Sort capacity-map keys.
- Normalize and sort request identifiers only where constraint semantics permit.
- Preserve potentially meaningful ordering and request identity.
- Serialize consistently.
- Do not include timestamps or nondeterministic values.

Avoid unnecessary DeviceClass updates when nothing relevant has changed.

**Added:** `internal/controller/deviceclass_manager.go` already has a `sortPartitionConfig` helper (sorts `SubResources` by `(DeviceClass, Count)`, sorts each `Alignments[i].Requests`, then sorts `Alignments` by `(Attribute, joined Requests)`) that runs on every `PartitionConfig` before it's marshaled into the DeviceClass's opaque config. By the time `BuildCapabilitySummary` receives its input, that input is already canonically ordered. **Do not have `BuildCapabilitySummary` define a second, independent sort order for the same logical fields** (`DeviceClass`/`Count`/`Attribute`/`Requests` map directly onto `ResourceCapability`/`TopologyCapability`) — two independently-hand-maintained orderings for the same data is exactly the kind of drift this requirement exists to prevent, and would be easy to let drift apart silently across future edits to either function. Factor the sort-key comparator out so both `sortPartitionConfig` and the capability-summary path call the same logic, or have `BuildCapabilitySummary` explicitly document that it trusts `sortPartitionConfig`'s invariant and re-sort its own output using literally the same key functions (not reimplemented ones).

### FR-10: Reconciliation

*(unchanged)*

### FR-11: Discovery

*(unchanged)*

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

**Placement: annotate every TC-managed DeviceClass this feature can reach — per-instance and aggregate alike — not aggregate-only.** `buildGroupingDeviceClass` is called identically for per-instance classes (`-rail<N>` names) and the aggregate class; `buildDeviceClassFromCache` is called identically for per-instance, partition-type-aggregate, and tier classes. Every one of these call sites already has a single, concrete, non-heterogeneous `PartitionConfig` in hand (per-instance classes are homogeneous by construction — one `GroupingInstance`/`PartitionDevice` each — so FR-5's heterogeneity concern cannot arise for them at all). This is a different placement decision than PR2's intent metadata (which is aggregate-only, a *presentation* choice to keep `kubectl get deviceclasses` output to one row per offering) — the capability summary is a per-object derived fact about that object's own resolved configuration, not administrator-authored prose, so there's no discovery-noise argument for omitting it from per-instance classes. Call `BuildCapabilitySummary` + annotate inside (or immediately around) both `buildGroupingDeviceClass` and `buildDeviceClassFromCache`, once per `*resourcev1.DeviceClass` object produced, regardless of which emission loop produced it.

---

## 8. Tests

*(unchanged, plus one addition)*

Add the existing 22 unit test cases, integration tests, and property/fuzz tests as originally specified.

**Added:** a unit test exercising `BuildCapabilitySummary` against a `PartitionConfig` derived from an intersected, asymmetric grouping aggregate (i.e., feed it the *output* of the now-fixed `SyncGroupingDeviceClasses` aggregate path for a grouping where rails differ), asserting the summary reflects only the intersected, guaranteed subset — this is the regression test that proves FR-5 end-to-end, not just at the `SyncGroupingDeviceClasses` layer (which already has its own regression test, `TestDeviceClassManager_SyncGroupingDeviceClasses_AggregateIntersectsAsymmetricRails`, added alongside the fix).

---

## 9. Documentation

*(unchanged)*

---

## 10. Non-Goals

*(unchanged)*

---

## 11. Acceptance Criteria

*(unchanged, all still apply; "Heterogeneous classes do not publish false uniform guarantees" is now achievable as stated rather than requiring a weaker implementation)*

---

## 12. Instructions for Claude Code / implementing agent

**[AMENDED]** §12's original "Second: Review design decisions" step asked the implementer to investigate whether aggregate DeviceClasses have a single authoritative PartitionConfig, and to document any cases requiring variants or conservative summaries. **That investigation has already been done and the finding has already been acted on** (see "Resolved blocker" at the top of this document) — do not re-investigate from scratch, but do re-verify the fix is present on whatever branch you're implementing against (`fix/grouping-aggregate-intersection`, or its merged state on `main`) before assuming FR-5 is satisfied for free. If that fix is not present on your base branch, implementing this feature without it will reproduce the exact problem this amendment documents.

Everything else in the original §12 (inspect first, implement, validate with `make test`/`make lint`/`make build`, report unavailable tests rather than claiming they passed, deliver the standard summary) is unchanged.

---

## 13. Future Extensions

*(unchanged)*
