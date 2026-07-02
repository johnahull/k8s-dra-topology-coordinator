// Package webhook implements a mutating admission webhook that expands
// partition ResourceClaims into multi-request claims with topology alignment.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	klog "k8s.io/klog/v2"

	"github.com/fabiendupont/k8s-dra-driver-nodepartition/internal/controller"
)

const (
	driverName = "nodepartition.dra.k8s.io"
)

// ClaimExpander is a mutating admission webhook that expands partition
// ResourceClaims into multi-request claims with alignment constraints.
type ClaimExpander struct {
	client  kubernetes.Interface
	decoder runtime.Decoder
	model   *controller.TopologyModel
}

// jsonPatch represents a single JSON Patch operation.
type jsonPatch struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// NewClaimExpander creates a new ClaimExpander webhook handler.
// The model parameter is optional; when provided, it enables satisfiability
// checks for "preferred" enforcement constraints.
func NewClaimExpander(client kubernetes.Interface, model ...*controller.TopologyModel) *ClaimExpander {
	scheme := runtime.NewScheme()
	codecs := serializer.NewCodecFactory(scheme)
	ce := &ClaimExpander{
		client:  client,
		decoder: codecs.UniversalDeserializer(),
	}
	if len(model) > 0 {
		ce.model = model[0]
	}
	return ce
}

// Handler returns the HTTP handler for the webhook.
func (ce *ClaimExpander) Handler() http.Handler {
	return ce
}

// ServeHTTP handles admission review requests.
func (ce *ClaimExpander) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		klog.Errorf("Failed to read request body: %v", err)
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil {
		klog.Errorf("Failed to unmarshal admission review: %v", err)
		http.Error(w, "failed to unmarshal admission review", http.StatusBadRequest)
		return
	}

	if review.Request == nil {
		klog.Error("Admission review has no request")
		http.Error(w, "admission review has no request", http.StatusBadRequest)
		return
	}

	response := ce.handleAdmission(r.Context(), review.Request)
	review.Response = response
	review.Response.UID = review.Request.UID

	respBytes, err := json.Marshal(review)
	if err != nil {
		klog.Errorf("Failed to marshal admission response: %v", err)
		http.Error(w, "failed to marshal response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBytes); err != nil {
		klog.Errorf("Failed to write admission response: %v", err)
	}
}

// handleAdmission processes a single admission request and returns the response.
func (ce *ClaimExpander) handleAdmission(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	// Route by resource type
	if req.Resource.Group == "" && req.Resource.Resource == "pods" {
		if req.Operation == admissionv1.Create {
			return ce.handlePodAdmission(ctx, req)
		}
		return allowResponse()
	}

	if req.Resource.Group == "kubevirt.io" && req.Resource.Resource == "virtualmachineinstances" {
		if req.Operation == admissionv1.Create {
			return ce.handleVMIAdmission(ctx, req)
		}
		return allowResponse()
	}

	// Only handle ResourceClaims
	if req.Resource.Group != "resource.k8s.io" || req.Resource.Resource != "resourceclaims" {
		return allowResponse()
	}

	// Only handle CREATE and UPDATE
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return allowResponse()
	}

	var claim resourcev1.ResourceClaim
	if err := json.Unmarshal(req.Object.Raw, &claim); err != nil {
		klog.Errorf("Failed to unmarshal ResourceClaim: %v", err)
		return allowResponse()
	}

	patches, err := ce.expandClaim(ctx, &claim)
	if err != nil {
		klog.Errorf("Failed to expand claim %s/%s: %v", claim.Namespace, claim.Name, err)
		return allowResponse()
	}

	if len(patches) == 0 {
		return allowResponse()
	}

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		klog.Errorf("Failed to marshal patches: %v", err)
		return allowResponse()
	}

	patchType := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		PatchType: &patchType,
		Patch:     patchBytes,
	}
}

// expandClaim examines a ResourceClaim and generates JSON patches to expand
// partition requests into sub-resource requests with alignment constraints.
func (ce *ClaimExpander) expandClaim(ctx context.Context, claim *resourcev1.ResourceClaim) ([]jsonPatch, error) { //nolint:unparam // error return reserved for future use
	requests := claim.Spec.Devices.Requests
	if len(requests) == 0 {
		return nil, nil
	}

	var expandedRequests []resourcev1.DeviceRequest
	var constraints []resourcev1.DeviceConstraint
	anyExpanded := false
	expansionMap := make(map[string][]string) // original request name → expanded request names

	for _, req := range requests {
		if req.Exactly == nil {
			expandedRequests = append(expandedRequests, req)
			continue
		}

		partitionConfig, err := ce.getPartitionConfig(ctx, req.Exactly.DeviceClassName)
		if err != nil {
			klog.Warningf("Failed to get partition config for DeviceClass %q: %v", req.Exactly.DeviceClassName, err)
			expandedRequests = append(expandedRequests, req)
			continue
		}

		if partitionConfig == nil {
			expandedRequests = append(expandedRequests, req)
			continue
		}

		// Expand this request into sub-resource requests
		subRequests, subConstraints := ce.expandRequest(req, partitionConfig)
		expandedRequests = append(expandedRequests, subRequests...)
		constraints = append(constraints, subConstraints...)
		anyExpanded = true

		// Track the expansion mapping for Pod rewriting
		var expandedNames []string
		for _, sr := range subRequests {
			expandedNames = append(expandedNames, sr.Name)
		}
		expansionMap[req.Name] = expandedNames
	}

	if !anyExpanded {
		return nil, nil
	}

	var patches []jsonPatch

	// Replace requests
	patches = append(patches, jsonPatch{
		Op:    "replace",
		Path:  "/spec/devices/requests",
		Value: expandedRequests,
	})

	// Add or replace constraints
	if len(constraints) > 0 {
		if len(claim.Spec.Devices.Constraints) == 0 {
			patches = append(patches, jsonPatch{
				Op:    "add",
				Path:  "/spec/devices/constraints",
				Value: constraints,
			})
		} else {
			merged := make([]resourcev1.DeviceConstraint, 0, len(claim.Spec.Devices.Constraints)+len(constraints))
			merged = append(merged, claim.Spec.Devices.Constraints...)
			merged = append(merged, constraints...)
			patches = append(patches, jsonPatch{
				Op:    "replace",
				Path:  "/spec/devices/constraints",
				Value: merged,
			})
		}
	}

	// Annotate the claim with the expansion mapping so the Pod webhook
	// can rewrite container request references without re-fetching DeviceClasses.
	if len(expansionMap) > 0 {
		mapJSON, err := json.Marshal(expansionMap)
		if err != nil {
			klog.Warningf("Failed to marshal expansion map: %v", err)
		} else {
			annotations := claim.Annotations
			if annotations == nil {
				annotations = make(map[string]string)
			}
			annotations[driverName+"/expanded-requests"] = string(mapJSON)
			if claim.Annotations == nil {
				patches = append(patches, jsonPatch{
					Op:    "add",
					Path:  "/metadata/annotations",
					Value: annotations,
				})
			} else {
				patches = append(patches, jsonPatch{
					Op:    "add",
					Path:  "/metadata/annotations/" + escapeJSONPointer(driverName+"/expanded-requests"),
					Value: string(mapJSON),
				})
			}
		}
	}

	return patches, nil
}

// expandRequest expands a partition DeviceRequest into sub-resource requests.
// When req.Exactly.Count > 1, creates N independent partition instances, each
// with its own set of sub-requests and alignment constraints.
func (ce *ClaimExpander) expandRequest(req resourcev1.DeviceRequest, config *controller.PartitionConfig) ([]resourcev1.DeviceRequest, []resourcev1.DeviceConstraint) {
	count := int64(1)
	if req.Exactly != nil && req.Exactly.Count > 1 {
		count = req.Exactly.Count
	}

	var allSubRequests []resourcev1.DeviceRequest
	var allConstraints []resourcev1.DeviceConstraint

	for i := int64(0); i < count; i++ {
		prefix := req.Name
		if count > 1 {
			prefix = fmt.Sprintf("%s-%d", req.Name, i)
		}
		subRequests, constraints := ce.expandSinglePartition(prefix, req, config)
		allSubRequests = append(allSubRequests, subRequests...)
		allConstraints = append(allConstraints, constraints...)
	}

	return allSubRequests, allConstraints
}

// expandSinglePartition expands one partition instance into sub-resource requests
// and alignment constraints. The prefix determines the naming of generated requests.
func (ce *ClaimExpander) expandSinglePartition(prefix string, req resourcev1.DeviceRequest, config *controller.PartitionConfig) ([]resourcev1.DeviceRequest, []resourcev1.DeviceConstraint) {
	var subRequests []resourcev1.DeviceRequest
	var constraints []resourcev1.DeviceConstraint

	// Build a map of generated request names for constraint resolution
	requestNameMap := make(map[string]string) // subresource device class -> generated request name

	for _, sr := range config.SubResources {
		sanitized := sanitizeDeviceClassName(sr.DeviceClass)
		name := prefix + "-" + sanitized
		requestNameMap[sr.DeviceClass] = name

		count := int64(sr.Count)
		exact := &resourcev1.ExactDeviceRequest{
			DeviceClassName: sr.DeviceClass,
			Count:           count,
		}

		// Add capacity requests for shared devices (DRAConsumableCapacity)
		if len(sr.Capacity) > 0 {
			exact.Capacity = &resourcev1.CapacityRequirements{
				Requests: make(map[resourcev1.QualifiedName]resource.Quantity),
			}
			for capName, capVal := range sr.Capacity {
				qty, err := resource.ParseQuantity(capVal)
				if err == nil {
					exact.Capacity.Requests[resourcev1.QualifiedName(capName)] = qty
				}
			}
		}

		// Apply per-driver CEL selectors from the PartitionConfig.
		// These pin each sub-request to the correct NUMA node using the
		// driver's own attribute namespace (e.g., gpu.amd.com/numaNode),
		// eliminating the need for a common cross-driver attribute name.
		for _, cel := range sr.Selectors {
			exact.Selectors = append(exact.Selectors, resourcev1.DeviceSelector{
				CEL: &resourcev1.CELDeviceSelector{
					Expression: cel,
				},
			})
		}

		// Forward any user-specified selectors from the original partition request
		// to each expanded sub-request. This enables NUMA pinning (e.g.,
		// numaNode==0) and other user-specified device filtering.
		if req.Exactly != nil && len(req.Exactly.Selectors) > 0 {
			exact.Selectors = append(exact.Selectors, req.Exactly.Selectors...)
		}

		subRequests = append(subRequests, resourcev1.DeviceRequest{
			Name:    name,
			Exactly: exact,
		})
	}

	// Build driver counts map for satisfiability checks
	driverCounts := make(map[string]int, len(config.SubResources))
	for _, sr := range config.SubResources {
		driverCounts[sr.DeviceClass] = sr.Count
	}

	// Build constraints from alignments
	for _, alignment := range config.Alignments {
		// Skip preferred constraints that cannot be satisfied
		if alignment.Enforcement == controller.EnforcementPreferred {
			if ce.model == nil || !ce.model.IsConstraintSatisfiable(alignment.Attribute, driverCounts) {
				klog.V(2).Infof("Skipping preferred constraint %s: not satisfiable (or no topology model)", alignment.Attribute)
				continue
			}
		}

		var resolvedRequests []string
		for _, reqName := range alignment.Requests {
			// Try to resolve the request name through the mapping
			if mapped, ok := requestNameMap[reqName]; ok {
				resolvedRequests = append(resolvedRequests, mapped)
			} else {
				// Use as-is (might be a reference like "partition" or a direct name)
				// Try prefixing with the original request name
				found := false
				for _, sr := range config.SubResources {
					sanitized := sanitizeDeviceClassName(sr.DeviceClass)
					if reqName == sanitized {
						resolvedRequests = append(resolvedRequests, prefix+"-"+sanitized)
						found = true
						break
					}
				}
				if !found {
					// Skip references to the original request name (e.g., "partition")
					// since it no longer exists after expansion
					continue
				}
			}
		}

		if len(resolvedRequests) < 2 {
			// A match constraint needs at least 2 requests to be meaningful
			continue
		}

		attr := resourcev1.FullyQualifiedName(alignment.Attribute)
		constraints = append(constraints, resourcev1.DeviceConstraint{
			Requests:       resolvedRequests,
			MatchAttribute: &attr,
		})
	}

	return subRequests, constraints
}

// getPartitionConfig looks up a DeviceClass and extracts the PartitionConfig
// from its opaque configuration if present.
func (ce *ClaimExpander) getPartitionConfig(ctx context.Context, deviceClassName string) (*controller.PartitionConfig, error) {
	dc, err := ce.client.ResourceV1().DeviceClasses().Get(ctx, deviceClassName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get DeviceClass %q: %w", deviceClassName, err)
	}

	for _, cfg := range dc.Spec.Config {
		if cfg.Opaque == nil {
			continue
		}
		if cfg.Opaque.Driver != driverName {
			continue
		}

		var partConfig controller.PartitionConfig
		if err := json.Unmarshal(cfg.Opaque.Parameters.Raw, &partConfig); err != nil {
			klog.Warningf("Failed to unmarshal opaque config for DeviceClass %q: %v", deviceClassName, err)
			continue
		}

		if partConfig.Kind != "PartitionConfig" {
			continue
		}

		return &partConfig, nil
	}

	return nil, nil
}

// sanitizeDeviceClassName converts a device class name into a DNS-label-safe suffix.
func sanitizeDeviceClassName(name string) string {
	sanitized := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return '-'
	}, name)

	// Collapse consecutive dashes
	for strings.Contains(sanitized, "--") {
		sanitized = strings.ReplaceAll(sanitized, "--", "-")
	}
	sanitized = strings.Trim(sanitized, "-")

	if sanitized == "" {
		sanitized = "sub"
	}

	return sanitized
}

// handlePodAdmission rewrites container resource claim request names to match
// expanded partition request names. This makes partition expansion transparent
// to consumers like KubeVirt that read KEP-5304 metadata by request name.
func (ce *ClaimExpander) handlePodAdmission(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		klog.Errorf("Failed to unmarshal Pod: %v", err)
		return allowResponse()
	}

	// Build a map of pod claim name → expansion mapping by looking up referenced claims
	type claimExpansion struct {
		mapping map[string][]string // original request → expanded requests
	}
	claimExpansions := make(map[string]*claimExpansion) // pod claim name → expansion

	for _, prc := range pod.Spec.ResourceClaims {
		// Resolve the claim name — direct reference or template-generated.
		// Template-generated claims use a naming convention: <pod-name>-<template-name>.
		// During CREATE admission the pod may not have a name yet (generateName),
		// so we try direct lookup first.
		var claimToLookup string
		if prc.ResourceClaimName != nil && *prc.ResourceClaimName != "" {
			claimToLookup = *prc.ResourceClaimName
		} else if prc.ResourceClaimTemplateName != nil && *prc.ResourceClaimTemplateName != "" {
			// For template claims, the ResourceClaim is created by the scheduler
			// with name <pod-name>-<claim-name>. Try looking up the template claim.
			claimToLookup = *prc.ResourceClaimTemplateName
		}
		if claimToLookup == "" {
			continue
		}

		claim, err := ce.client.ResourceV1().ResourceClaims(req.Namespace).Get(ctx, claimToLookup, metav1.GetOptions{})
		if err != nil {
			klog.V(4).Infof("Could not look up claim %s/%s for pod rewriting: %v", req.Namespace, claimToLookup, err)
			continue
		}

		annotation := claim.Annotations[driverName+"/expanded-requests"]
		if annotation == "" {
			continue
		}

		var mapping map[string][]string
		if err := json.Unmarshal([]byte(annotation), &mapping); err != nil {
			klog.Warningf("Failed to parse expansion annotation on claim %s: %v", claimToLookup, err)
			continue
		}

		claimExpansions[prc.Name] = &claimExpansion{mapping: mapping}
	}

	if len(claimExpansions) == 0 {
		return allowResponse()
	}

	var patches []jsonPatch

	// Rewrite container claim references
	rewriteContainerClaims := func(containerPath string, containers []corev1.Container) {
		for i, ctr := range containers {
			var newClaims []corev1.ResourceClaim
			changed := false
			for _, rc := range ctr.Resources.Claims {
				exp, ok := claimExpansions[rc.Name]
				if !ok || rc.Request == "" {
					newClaims = append(newClaims, rc)
					continue
				}
				expandedNames, ok := exp.mapping[rc.Request]
				if !ok {
					newClaims = append(newClaims, rc)
					continue
				}
				// Expand into N entries, one per sub-request
				for _, expandedName := range expandedNames {
					newClaims = append(newClaims, corev1.ResourceClaim{
						Name:    rc.Name,
						Request: expandedName,
					})
				}
				changed = true
			}
			if changed {
				patches = append(patches, jsonPatch{
					Op:    "replace",
					Path:  fmt.Sprintf("%s/%d/resources/claims", containerPath, i),
					Value: newClaims,
				})
			}
		}
	}

	rewriteContainerClaims("/spec/containers", pod.Spec.Containers)
	rewriteContainerClaims("/spec/initContainers", pod.Spec.InitContainers)

	if len(patches) == 0 {
		return allowResponse()
	}

	podID := pod.Name
	if podID == "" {
		podID = pod.GenerateName + "*"
	}
	klog.Infof("Rewriting %d container claim references in pod %s/%s", len(patches), req.Namespace, podID)

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		klog.Errorf("Failed to marshal pod patches: %v", err)
		return allowResponse()
	}

	patchType := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		PatchType: &patchType,
		Patch:     patchBytes,
	}
}

// handleVMIAdmission rewrites GPU and HostDevice requestName fields in a
// KubeVirt VirtualMachineInstance to match expanded partition request names.
func (ce *ClaimExpander) handleVMIAdmission(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	var vmi map[string]interface{}
	if err := json.Unmarshal(req.Object.Raw, &vmi); err != nil {
		klog.Errorf("Failed to unmarshal VMI: %v", err)
		return allowResponse()
	}

	// Build expansion map from referenced claims
	expansionMap := make(map[string]map[string][]string) // claim name -> original request -> expanded requests

	resourceClaims, _ := nestedSlice(vmi, "spec", "resourceClaims")
	for _, rc := range resourceClaims {
		rcMap, ok := rc.(map[string]interface{})
		if !ok {
			continue
		}
		claimName, _ := rcMap["resourceClaimName"].(string)
		if claimName == "" {
			continue
		}

		claim, err := ce.client.ResourceV1().ResourceClaims(req.Namespace).Get(ctx, claimName, metav1.GetOptions{})
		if err != nil {
			continue
		}

		annotation := claim.Annotations[driverName+"/expanded-requests"]
		if annotation == "" {
			continue
		}

		var mapping map[string][]string
		if err := json.Unmarshal([]byte(annotation), &mapping); err != nil {
			continue
		}

		rcName, _ := rcMap["name"].(string)
		expansionMap[rcName] = mapping
	}

	if len(expansionMap) == 0 {
		return allowResponse()
	}

	var patches []jsonPatch

	// Rewrite gpus[].requestName
	gpus, _ := nestedSlice(vmi, "spec", "domain", "devices", "gpus")
	for i, g := range gpus {
		gMap, ok := g.(map[string]interface{})
		if !ok {
			continue
		}
		claimName, _ := gMap["claimName"].(string)
		requestName, _ := gMap["requestName"].(string)
		if claimName == "" || requestName == "" {
			continue
		}
		mapping, ok := expansionMap[claimName]
		if !ok {
			continue
		}
		expanded, ok := mapping[requestName]
		if !ok || len(expanded) == 0 {
			continue
		}
		// Find the GPU sub-request (contains "gpu" in the name)
		for _, name := range expanded {
			if strings.Contains(name, "gpu") {
				patches = append(patches, jsonPatch{
					Op:    "replace",
					Path:  fmt.Sprintf("/spec/domain/devices/gpus/%d/requestName", i),
					Value: name,
				})
				break
			}
		}
	}

	// Rewrite hostDevices[].requestName
	hostDevices, _ := nestedSlice(vmi, "spec", "domain", "devices", "hostDevices")
	for i, hd := range hostDevices {
		hdMap, ok := hd.(map[string]interface{})
		if !ok {
			continue
		}
		claimName, _ := hdMap["claimName"].(string)
		requestName, _ := hdMap["requestName"].(string)
		if claimName == "" || requestName == "" {
			continue
		}
		mapping, ok := expansionMap[claimName]
		if !ok {
			continue
		}
		expanded, ok := mapping[requestName]
		if !ok || len(expanded) == 0 {
			continue
		}
		// For hostDevices, use the device name hint to match
		deviceName, _ := hdMap["name"].(string)
		matched := false
		for _, name := range expanded {
			if strings.Contains(name, "nvme") && strings.Contains(deviceName, "nvme") {
				patches = append(patches, jsonPatch{
					Op:    "replace",
					Path:  fmt.Sprintf("/spec/domain/devices/hostDevices/%d/requestName", i),
					Value: name,
				})
				matched = true
				break
			}
			if strings.Contains(name, "net") && strings.Contains(deviceName, "nic") {
				patches = append(patches, jsonPatch{
					Op:    "replace",
					Path:  fmt.Sprintf("/spec/domain/devices/hostDevices/%d/requestName", i),
					Value: name,
				})
				matched = true
				break
			}
			if strings.Contains(name, "sriov") && strings.Contains(deviceName, "nic") {
				patches = append(patches, jsonPatch{
					Op:    "replace",
					Path:  fmt.Sprintf("/spec/domain/devices/hostDevices/%d/requestName", i),
					Value: name,
				})
				matched = true
				break
			}
		}
		if !matched {
			// Fallback: use first non-GPU expanded name
			for _, name := range expanded {
				if !strings.Contains(name, "gpu") {
					patches = append(patches, jsonPatch{
						Op:    "replace",
						Path:  fmt.Sprintf("/spec/domain/devices/hostDevices/%d/requestName", i),
						Value: name,
					})
					break
				}
			}
		}
	}

	if len(patches) == 0 {
		return allowResponse()
	}

	vmiName, _ := vmi["metadata"].(map[string]interface{})["name"].(string)
	klog.Infof("Rewriting %d device request names in VMI %s/%s", len(patches), req.Namespace, vmiName)

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		klog.Errorf("Failed to marshal VMI patches: %v", err)
		return allowResponse()
	}

	patchType := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		PatchType: &patchType,
		Patch:     patchBytes,
	}
}

// nestedSlice extracts a nested []interface{} from a map hierarchy.
func nestedSlice(obj map[string]interface{}, fields ...string) ([]interface{}, bool) {
	current := obj
	for i, field := range fields {
		if i == len(fields)-1 {
			val, ok := current[field].([]interface{})
			return val, ok
		}
		next, ok := current[field].(map[string]interface{})
		if !ok {
			return nil, false
		}
		current = next
	}
	return nil, false
}

// escapeJSONPointer escapes special characters in JSON Pointer tokens (RFC 6901).
func escapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}

// allowResponse returns an admission response that allows the request without mutation.
func allowResponse() *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: true,
	}
}
