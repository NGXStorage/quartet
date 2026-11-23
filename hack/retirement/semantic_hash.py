#!/usr/bin/env python3
"""Shared semantic-hash contract for retirement tooling (
review).

One normalization + hash implementation is used by inventory.sh,
make-execution-manifest consumers, retire.py postconditions AND the test
manifest builders — a second, divergent hash algorithm is a contract
violation. Bump CONTRACT_VERSION and re-scan when the field set changes.

Normalization:
  - metadata: uid, resourceVersion, managedFields, creationTimestamp,
    selfLink, generation removed (server-owned / monotonically bumped).
  - status removed.
  - Secret data/stringData removed and the hash string gets the
    "(data-excluded)" suffix — credential values never enter any hash,
    inventory, or evidence.
  - Lease spec.renewTime removed (the only documented volatile field:
    sidecars renew leases in place; everything else in the spec is
    compared).
"""
import copy, hashlib, json

CONTRACT_VERSION = "v1"

_SERVER_FIELDS = ("uid", "resourceVersion", "managedFields",
                  "creationTimestamp", "selfLink", "generation")
_DATA_EXCLUDED = "(data-excluded)"


def strip_object(o, kind):
    o = copy.deepcopy(o)
    md = o.get("metadata", {})
    for f in _SERVER_FIELDS:
        md.pop(f, None)
    o.pop("status", None)
    k = (kind or "").lower()
    if k == "secret":
        o.pop("data", None)
        o.pop("stringData", None)
    if k == "lease" and isinstance(o.get("spec"), dict):
        o["spec"].pop("renewTime", None)
    return o


def semantic_hash(o, kind):
    h = hashlib.sha256(json.dumps(strip_object(o, kind), sort_keys=True)
                       .encode()).hexdigest()
    if (kind or "").lower() == "secret":
        h += _DATA_EXCLUDED
    return h
