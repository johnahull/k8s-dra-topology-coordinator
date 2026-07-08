#!/bin/bash
# Show PartitionConfig from a DeviceClass
DC=$1
kubectl get deviceclass "$DC" -o json | python3 -c "
import json,sys
dc=json.load(sys.stdin)
for cfg in dc['spec']['config']:
    if 'opaque' not in cfg:
        continue
    p=cfg['opaque'].get('parameters',{})
    if isinstance(p,str):
        p=json.loads(p)
    if p.get('kind')=='PartitionConfig':
        print(json.dumps(p, indent=2))
" | head -50
