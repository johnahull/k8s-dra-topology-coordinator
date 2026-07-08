#!/bin/bash
# Show expanded claim sub-requests and alignment constraints
CLAIM=$1
kubectl get resourceclaim "$CLAIM" -o json | python3 -c "
import json,sys
c=json.load(sys.stdin)
reqs=c['spec']['devices'].get('requests',[])
cons=c['spec']['devices'].get('constraints',[])
print(f'{len(reqs)} sub-requests:')
for r in reqs:
    e=r.get('exactly',{})
    print(f'  {r[\"name\"]}: {e.get(\"deviceClassName\",\"?\")}')
print(f'{len(cons)} alignment constraints:')
for c2 in cons:
    ma=c2.get('matchAttribute','').split('/')[-1]
    reqs2=c2.get('requests',[])
    print(f'  {ma}: [{\", \".join(reqs2)}]')
"
