package controller

import (
	"bytes"
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	klog "k8s.io/klog/v2"
)

func captureKlog(t *testing.T, call func()) string {
	t.Helper()
	var output bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(&output)
	t.Cleanup(func() {
		klog.SetOutput(os.Stderr)
		klog.LogToStderr(true)
	})
	call()
	return output.String()
}

func TestController_Reconcile_DuplicateUserNamesLogsActionableError(t *testing.T) {
	ctrl := NewController(k8sfake.NewSimpleClientset(), CoordinatorDriverName, PartitionModeGroupings)
	for _, cmName := range []string{"first", "second"} {
		require.NoError(t, ctrl.groupingStore.LoadFromConfigMap(
			makeGroupingConfigMap(cmName, validGroupingData("shared-offering")),
		))
	}

	logs := captureKlog(t, func() {
		require.NoError(t, ctrl.reconcile(context.Background()))
	})

	assert.Contains(t, logs, "data.name")
	assert.Contains(t, logs, "shared-offering")
	assert.Contains(t, logs, "default/first")
	assert.Contains(t, logs, "default/second")
}

func TestController_Reconcile_SanitizedCollisionPreservesExistingDeviceClass(t *testing.T) {
	existing := &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gpu-inference",
			Labels: map[string]string{
				CoordinatorDriverName + "/managed":  "true",
				CoordinatorDriverName + "/grouping": "gpu-inference",
			},
		},
	}
	client := k8sfake.NewSimpleClientset(existing)
	ctrl := NewController(client, CoordinatorDriverName, PartitionModeGroupings)
	for _, groupingName := range []string{"gpu.inference", "gpu-inference"} {
		cmName := strings.ReplaceAll(groupingName, ".", "-dot-")
		data := validGroupingData(groupingName)
		require.NoError(t, ctrl.groupingStore.LoadFromConfigMap(makeGroupingConfigMap(cmName, data)))
	}

	err := ctrl.reconcile(context.Background())
	assert.Error(t, err)
	if err != nil {
		for _, want := range []string{"data.name", "gpu.inference", "gpu-inference", "default/gpu-dot-inference", "default/gpu-inference"} {
			assert.Contains(t, err.Error(), want)
		}
	}

	after, getErr := client.ResourceV1().DeviceClasses().Get(context.Background(), existing.Name, metav1.GetOptions{})
	require.NoError(t, getErr, "a sanitized-name collision must not run stale-class cleanup")
	assert.Equal(t, existing.Labels, after.Labels)
}

func TestGroupingConfigMapUpdate_InvalidUpdateKeepsCachedGrouping(t *testing.T) {
	ctrl := NewController(k8sfake.NewSimpleClientset(), CoordinatorDriverName, PartitionModeGroupings)
	original := makeGroupingConfigMap("test", validGroupingData("existing-offering"))
	require.NoError(t, ctrl.groupingStore.LoadFromConfigMap(original))
	updated := original.DeepCopy()
	updated.Data["intentLevel"] = "unknown"

	logs := captureKlog(t, func() {
		ctrl.onGroupingConfigMapUpdate(original, updated)
	})

	assert.Contains(t, logs, "default/test")
	assert.Contains(t, logs, "stale")
	groupings := ctrl.groupingStore.GetGroupings()
	require.Len(t, groupings, 1)
	assert.Equal(t, "existing-offering", groupings[0].Name)
}

func TestMergeGroupings_UserOverridesAuto(t *testing.T) {
	auto := []DeviceGrouping{
		{Name: "gpu-nic-pair", Alignment: "pcieRoot", Devices: []GroupingDevice{{Class: "gpu.auto", Count: 1}}},
	}
	user := []DeviceGrouping{
		{Name: "gpu-nic-pair", Alignment: "numaNode", Devices: []GroupingDevice{{Class: "gpu.user", Count: 2}}},
	}

	merged := mergeGroupings(auto, user)
	assert.Len(t, merged, 1)
	assert.Equal(t, "numaNode", merged[0].Alignment, "user-defined should override auto-detected")
	assert.Equal(t, "gpu.user", merged[0].Devices[0].Class)
}

func TestMergeGroupings_BothEmpty(t *testing.T) {
	merged := mergeGroupings(nil, nil)
	assert.Empty(t, merged)
}

func TestMergeGroupings_NoOverlap(t *testing.T) {
	auto := []DeviceGrouping{
		{Name: "auto-pair", Alignment: "pcieRoot"},
	}
	user := []DeviceGrouping{
		{Name: "user-pair", Alignment: "numaNode"},
	}

	merged := mergeGroupings(auto, user)
	assert.Len(t, merged, 2)

	names := make([]string, len(merged))
	for i, g := range merged {
		names[i] = g.Name
	}
	sort.Strings(names)
	assert.Equal(t, []string{"auto-pair", "user-pair"}, names)
}

func TestMergeGroupings_AutoOnly(t *testing.T) {
	auto := []DeviceGrouping{
		{Name: "pair-a"},
		{Name: "pair-b"},
	}
	merged := mergeGroupings(auto, nil)
	assert.Len(t, merged, 2)
}

func TestMergeGroupings_UserOnly(t *testing.T) {
	user := []DeviceGrouping{
		{Name: "pair-a"},
	}
	merged := mergeGroupings(nil, user)
	assert.Len(t, merged, 1)
}
