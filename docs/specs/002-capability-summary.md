# Implementation Specification: Automatically Generated Hardware Capability Summaries

**Project:** `rh-ecosystem-edge/k8s-dra-topology-coordinator`
**Feature:** Derived Hardware Capability Summaries
**Status:** Proposed — review amendment recorded; not implementation-ready until the open items below are resolved
**Prerequisite:** Intent-Based Hardware Offerings metadata enhancement (PR2, #13, not yet merged to `main` as of this amendment); **and** the grouping-aggregate count intersection fix (PR #14, https://github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/pull/14, not yet merged — see below) — this feature cannot be safely implemented without it.
**Initial scope:** Kubernetes DRA DeviceClasses
**Priority:** High

---

## Amendment note (read this first)

This spec was reviewed against the current code. The amendments below record the resolved device-count issue and the remaining implementation blockers. Sections marked `unchanged` are placeholders: their original text is not included here, and no earlier copy was found in this repository history. Restore that content or replace each placeholder with complete requirements before implementation.

### Readiness blockers

1. Aggregate DeviceClass counts are intersected, but capacity values are copied from one qualifying instance without comparison. Before aggregate summaries are published, the aggregation path must establish that each retained resource's capacity map is identical across all alternatives; otherwise, omit both summary annotations for that aggregate and surface an error.
2. The 32 KiB summary-pair limit does not ensure that the final merged annotation map stays below Kubernetes' combined annotation limit. The check must account for existing foreign and TC-owned annotations at publish time.
3. The omitted `unchanged` sections and original 22-case test inventory are not available in this checkout. They must be restored or rewritten before this document is an executable implementation specification.

### Resolved device-count blocker: grouping aggregates did not intersect

The original §6 FR-5 assumed "the final PartitionConfig associated with each generated class" was always a trustworthy, guaranteed-across-every-realization description for aggregate classes. It was not, for one specific aggregate type.

Verified in `internal/controller/deviceclass_manager.go`: `SyncDeviceClasses`'s partition-type aggregate (the pcieroot/numa aggregate, and by inheritance the tier-named aggregates) intersects driver counts — a driver's count only survives if it's present on *every* partition of that type (`driverSeen[driver] == total`), at the minimum count across instances, with a System Locality Information Table (SLIT) distance-based reachability carve-out for NUMA-shareable drivers. This establishes a count guarantee; it does not establish that capacity values are identical or conservative across those partitions.

`SyncGroupingDeviceClasses`'s grouping aggregate (the one named after the grouping itself, e.g. `gpu-nic-pair`) did **not** do this. It picked one arbitrary instance — deterministically the lowest `RailIndex`, as of a prior correctness fix — and published that single instance's `PartitionConfig` as the entire aggregate's config. If the grouping is asymmetric across rails (devices present on some rails but not others — explicitly the reason groupings exist, per the code's own comment on the partition-aggregate intersection: "Asymmetric devices... handled via grouping DeviceClasses instead"), the grouping aggregate's capability summary would describe one rail's hardware as if it were guaranteed for the whole grouping. A pure `BuildCapabilitySummary(config PartitionConfig) (CapabilitySummary, error)` has no way to detect or correct this from inside the function — `PartitionConfig` carries no provenance about whether it was intersected or arbitrarily picked, so FR-5's "if PartitionConfig does not contain enough information to distinguish alternatives, report that limitation" could not have been implemented against the original code no matter how carefully `BuildCapabilitySummary` was written.

**Fix (committed on branch `fix/grouping-aggregate-intersection`):** `SyncGroupingDeviceClasses` now intersects driver presence and counts across instances of a grouping name, retaining the minimum count. Capacity is not intersected: the accumulator copies the last qualifying instance's capacity map without comparing it, matching the existing partition-aggregate behavior. The NUMA-SLIT-reachability carve-out was deliberately **not** ported — it's a partition-type-specific optimization tied to NUMA distance semantics with no equivalent in grouping alignment (pcieRoot/numaNode/socket are a matching key here, not a distance model). The existing tests cover asymmetric driver counts, not differing capacity values.

**Consequence for this spec:** Driver presence and counts are intersected for current aggregate types. Capacity values are not proven uniform: the code retains a last-seen capacity map, and PCIe partition capacities can differ when the capacity is divided by different root counts. Apply this policy before implementing aggregate summaries: include capacity only when every alternative has an identical capacity map for each resource retained in the aggregate. Compare keys and string values exactly; do not choose a last-seen or minimum value. If any such map differs, or equality cannot be established, omit both capability summary annotations for that aggregate and surface an error naming the DeviceClass while still reconciling its `Spec.Config` and other metadata. Per-instance summaries copy their configured capacity unchanged. Track this result while alternatives are available because `BuildCapabilitySummary(PartitionConfig)` cannot infer it from the final config alone. This is an implementation blocker, not a future-only concern.

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

Original defensive language (retain as a durable invariant; count guarantees are enforced at the source, but capacity guarantees remain unresolved):

> If a class represents alternative physical realizations with different underlying configurations, do not publish one representative configuration as though it is guaranteed for every realization. For heterogeneous classes, either (1) publish explicitly represented configuration variants, with their applicability, or (2) publish only characteristics guaranteed across every alternative and clearly mark unresolved details. If the existing PartitionConfig does not contain enough information to distinguish alternatives, report that limitation rather than inferring uniformity.

**Status as of this amendment:** device presence and counts satisfy option (2) for current aggregate types. Capacity does not yet satisfy the guarantee:

- Partition-type aggregates (pcieroot/numa) and tier-named aggregates: intersect driver presence and counts, with an SLIT-reachability carve-out for NUMA-shareable drivers. Capacity maps are copied from the last qualifying partition; they are not compared.
- Grouping aggregates: intersect driver presence and counts (minus the NUMA-SLIT carve-out, which has no grouping-alignment equivalent). Capacity maps are likewise copied from the last qualifying instance; they are not compared.

`BuildCapabilitySummary` cannot determine whether an aggregate capacity is uniform from `PartitionConfig` alone. The aggregate-building path must pass that result to summary generation. If the retained resource capacity maps are not identical across alternatives, omit both summary annotations for the aggregate and log a surfaced error; continue publishing the DeviceClass's `Spec.Config` and other metadata. This policy requires no uncertainty field in the public schema and does not change claim expansion.

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
- Treat `AlignmentConfig.Requests` as a set of request identifiers and sort them lexically; retain request identity.
- Preserve ordering for every other list unless its semantics are explicitly documented as order-independent.
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

**Placement: annotate every TC-managed DeviceClass this feature can reach — per-instance and aggregate alike — not aggregate-only.** `buildGroupingDeviceClass` is called identically for per-instance classes (`-rail<N>` names) and the aggregate class; `buildDeviceClassFromCache` is called identically for per-instance, partition-type-aggregate, and tier classes. Per-instance configs are homogeneous by construction — one `GroupingInstance`/`PartitionDevice` each — so FR-5's alternative-realization concern does not arise there. For aggregates, summary generation must receive the capacity-uniformity result from the aggregate-building path; if the retained resource capacity maps differ across alternatives, omit the summary annotations and surface an error. This is a different placement decision than PR2's intent metadata (which is aggregate-only, a *presentation* choice to keep `kubectl get deviceclasses` output to one row per offering) — the capability summary is a per-object derived fact about that object's own resolved configuration, not administrator-authored prose, so there's no discovery-noise argument for omitting it from per-instance classes. Call `BuildCapabilitySummary` + annotate inside (or immediately around) both `buildGroupingDeviceClass` and `buildDeviceClassFromCache`, once per `*resourcev1.DeviceClass` object produced, regardless of which emission loop produced it.

---

## 8. Tests

*(unchanged, plus one addition)*

The original 22-case unit-test inventory, integration tests, and property/fuzz tests are not included in this document. Restore or enumerate those cases before implementation; the review-specific coverage below is additive.

Add tests that cover:

- Equal capacity maps across aggregate alternatives are included unchanged; differing maps (including missing keys) omit both summary annotations and surface an error while the DeviceClass config is still reconciled.
- Per-instance summaries preserve the configured capacity map unchanged.
- Determinism when aggregate input order changes and capacities differ.
- A merged annotation map near the 256 KiB limit, including foreign annotations; verify the summary is omitted and the rest of the DeviceClass update proceeds.
- Oversized and unserializable summaries; verify stale summary keys are removed while the DeviceClass configuration is still reconciled.

**Added:** a unit test exercising `BuildCapabilitySummary` against a `PartitionConfig` derived from an intersected, asymmetric grouping aggregate (i.e., feed it the *output* of the now-fixed `SyncGroupingDeviceClasses` aggregate path for a grouping where rails differ), asserting the summary reflects the intersected driver/count subset. Capacity assertions follow the policy above: include identical maps and suppress the aggregate summary when maps differ. This tests the count intersection end-to-end, not just at the `SyncGroupingDeviceClasses` layer (which already has its own regression test, `TestDeviceClassManager_SyncGroupingDeviceClasses_AggregateIntersectsAsymmetricRails`, added alongside the fix).

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

**[AMENDED]** §12's original "Second: Review design decisions" step asked the implementer to investigate whether aggregate DeviceClasses have a single authoritative `PartitionConfig`, and to document any cases requiring variants or conservative summaries. The grouping driver/count intersection is implemented on `fix/grouping-aggregate-intersection`; verify that fix is present on the implementation base. **Do not assume FR-5 is fully satisfied:** implement the capacity-map equality check and summary omission policy above, plus the annotation-budget check, before publishing aggregate summaries.

Everything else in the original §12 (inspect first, implement, validate with `make test`/`make lint`/`make build`, report unavailable tests rather than claiming they passed, deliver the standard summary) is unchanged.

---

## 13. Future Extensions

*(unchanged)*
