# DRA Topology Coordinator Feature Demo
# Run: vhs demo.tape

Output demo.gif
Output demo.mp4

Set Shell "bash"
Set FontSize 14
Set Width 1400
Set Height 1200
Set Padding 10
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms

Hide
Type "export PATH=$HOME/devel/dra-topology-aware-co-placement/testing/scripts:$PATH"
Enter
Type "export DEMO=$HOME/devel/dra-topology-aware-co-placement/demos/topology-coordinator-features"
Enter
Sleep 1s
Type "clear"
Enter
Sleep 0.5s
Show

# 1. Hardware topology — Socket/NUMA/PCIeRoot/Device tree
Type "dra-verify.sh topology | more"
Enter
Sleep 8s
Type " "
Sleep 8s

# 2. ResourceSlice hardware summary
Type "dra-verify.sh slices"
Enter
Sleep 8s

# 3. Topology attributes per driver
Type "dra-verify.sh attributes | more"
Enter
Sleep 8s
Type " "
Sleep 8s

# 4. Active DRA drivers
Type "dra-verify.sh drivers"
Enter
Sleep 8s

# 5. Partition DeviceClasses — pcieroot, numa, full with sub-resources
Type "dra-verify.sh deviceclasses | more"
Enter
Sleep 10s
Type " "
Sleep 10s

# 6. All DeviceClasses — tier-named aggregates and pairings
Type "kubectl get deviceclass -o name | sort"
Enter
Sleep 8s

# 7. Auto-detected GPU+NIC pairings with rail index
Type "kubectl get deviceclass -o name | grep pair"
Enter
Sleep 6s

# 8. SLIT-aware reachability — numa aggregate config
Type "bash $DEMO/show-config.sh numa"
Enter
Sleep 10s

# 9. Webhook claim expansion — show and apply a claim for 1x eighth
Type "cat $DEMO/demo-claim-eighth.yaml"
Enter
Sleep 5s

Type "kubectl apply -f $DEMO/demo-claim-eighth.yaml"
Enter
Sleep 3s

# 10. Show expanded claim — webhook split into sub-requests + alignment
Type "bash $DEMO/show-claim.sh demo-eighth"
Enter
Sleep 8s

Hide
Type "kubectl delete resourceclaim demo-eighth 2>/dev/null"
Enter
Sleep 1s
Show

# 11. Multi-partition claim — show and apply count=3
Type "cat $DEMO/demo-claim-3eighths.yaml"
Enter
Sleep 5s

Type "kubectl apply -f $DEMO/demo-claim-3eighths.yaml"
Enter
Sleep 3s

# 12. Show expanded multi-claim — 3 partition groups
Type "bash $DEMO/show-claim.sh demo-3-eighths"
Enter
Sleep 8s

Hide
Type "kubectl delete resourceclaim demo-3-eighths 2>/dev/null"
Enter
Sleep 1s
Show

# 13. Deploy pods — show YAML then apply
Type "cat $DEMO/demo-pod-eighth.yaml"
Enter
Sleep 5s

Type "kubectl apply -f $DEMO/demo-pod-eighth.yaml"
Enter
Sleep 5s

Type "cat $DEMO/demo-pod-quarter.yaml"
Enter
Sleep 5s

Type "kubectl apply -f $DEMO/demo-pod-quarter.yaml"
Enter
Sleep 5s

Type "cat $DEMO/demo-pod-2eighth.yaml"
Enter
Sleep 5s

Type "kubectl apply -f $DEMO/demo-pod-2eighth.yaml"
Enter
Sleep 5s

# 14. Wait for pods to schedule
Type "kubectl get pods demo-pod-eighth demo-pod-quarter demo-pod-2eighth"
Enter
Sleep 5s

# 15. Show allocated claims with topology alignment
Type "dra-verify.sh claims | more"
Enter
Sleep 10s
Type " "
Sleep 10s

# 16. Show device class availability (some now in use)
Type "dra-verify.sh deviceclasses | more"
Enter
Sleep 10s
Type " "
Sleep 10s

# Cleanup
Hide
Type "kubectl delete pod demo-pod-eighth demo-pod-quarter demo-pod-2eighth 2>/dev/null"
Enter
Sleep 2s
Type "kubectl delete resourceclaim --all 2>/dev/null"
Enter
Sleep 2s
Type "kubectl delete resourceclaimtemplate demo-eighth-tpl demo-quarter-tpl demo-2eighth-tpl 2>/dev/null"
Enter
Show

Sleep 2s
