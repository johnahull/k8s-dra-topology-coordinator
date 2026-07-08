#!/bin/bash
# Show labels on a DeviceClass
DC=$1
kubectl get deviceclass "$DC" -o json | python3 -c "
import json,sys
labels=json.load(sys.stdin)['metadata']['labels']
for k,v in sorted(labels.items()):
    print(f'  {k} = {v}')
"
