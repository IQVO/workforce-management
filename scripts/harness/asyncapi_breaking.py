#!/usr/bin/env python3
"""asyncapi_breaking: breaking-change gate for apis/asyncapi.yaml (producer side).

Compares a revision against the base spec and fails (exit 1) on:
  1. a removed message (a components.messages entry that is gone);
  2. a removed or renamed REQUIRED data field of a surviving message's
     payload (the CloudEvents `data` object loses a required property);
  3. type narrowing (a data field's `type` set shrinks: [string, "null"]
     -> string; string -> integer fails the same superset rule).

Additions (a new message, a new optional field, a wider type) are NOT
breaking: the fleet contract is additive-only — a breaking payload
change ships as a new `.v2` message type and dataschema version, never
a mutation (ADR-0026). Consumers pin byte-copies of this spec at ref
develop (warehouse-docs network/model/contract-edges.json): warehouse-
planning and wes-work-planning, each at internal/adapters/inbound/
kafka/testdata/workforce-management.asyncapi.yaml.

Usage:  asyncapi_breaking.py BASE_YAML REVISION_YAML   0 compatible, 1 breaking
        asyncapi_breaking.py --self-check              embedded fixtures
"""
import sys

import yaml


def parts(doc):
    return doc.get("components") or {}


def resolve(schema, pool):
    """Follow one $ref level into the local schema pool."""
    if isinstance(schema, dict) and "$ref" in schema:
        return pool.get(str(schema["$ref"]).rsplit("/", 1)[-1], {})
    return schema if isinstance(schema, dict) else {}


def flatten(schema, pool):
    """Collapse a schema (allOf included) into (required, properties)."""
    req, props = set(), {}
    for chunk in [schema] + (schema.get("allOf") or []):
        chunk = resolve(chunk, pool)
        req |= set(chunk.get("required") or [])
        props.update(chunk.get("properties") or {})
    return req, props


def types_of(schema):
    t = schema.get("type")
    if isinstance(t, list):
        return set(t)
    if isinstance(t, str):
        return {t}
    return None  # untyped: shape change beyond `type` is not checked here


def data_schema(doc, message):
    """Resolve a message's payload down to its `data` object schema."""
    pool = parts(doc).get("schemas") or {}
    _, props = flatten(resolve(message.get("payload") or {}, pool), pool)
    return resolve(props.get("data") or {}, pool)


def breaking(base, rev):
    """Yield one human-readable finding per breaking change rev makes to base."""
    bmsg, rmsg = parts(base).get("messages") or {}, parts(rev).get("messages") or {}
    for name in sorted(bmsg):
        if name not in rmsg:
            yield f"message removed: {name}"
            continue
        pool_b, pool_r = parts(base).get("schemas") or {}, parts(rev).get("schemas") or {}
        breq, bprops = flatten(data_schema(base, bmsg[name]), pool_b)
        _, rprops = flatten(data_schema(rev, rmsg[name]), pool_r)
        for field in sorted(breq):
            if field not in rprops:
                yield f"{name}.data: required field removed or renamed: {field}"
        for field, bschema in sorted(bprops.items()):
            if field not in rprops:
                continue
            btypes, rtypes = types_of(bschema), types_of(rprops[field])
            if btypes is not None and rtypes is not None and not btypes <= rtypes:
                yield f"{name}.data.{field}: type narrowed: {sorted(btypes)} -> {sorted(rtypes)}"


BASE = """
components:
  messages:
    KeptAsIs: {payload: {$ref: '#/components/schemas/KeptEvent'}}
    GoneInRev: {payload: {$ref: '#/components/schemas/GoneEvent'}}
    FieldRenamed: {payload: {$ref: '#/components/schemas/RenamedEvent'}}
    TypeNarrowed: {payload: {$ref: '#/components/schemas/NarrowEvent'}}
  schemas:
    KeptEvent:
      allOf:
        - {required: [data], properties: {data: {$ref: '#/components/schemas/KeptData'}}}
    KeptData:
      required: [site_code, note]
      properties:
        site_code: {type: string}
        note: {type: [string, 'null']}
    GoneEvent:
      allOf:
        - {required: [data], properties: {data: {$ref: '#/components/schemas/GoneData'}}}
    GoneData: {required: [x], properties: {x: {type: string}}}
    RenamedEvent:
      allOf:
        - {required: [data], properties: {data: {$ref: '#/components/schemas/RenamedData'}}}
    RenamedData:
      required: [site_code]
      properties: {site_code: {type: string}}
    NarrowEvent:
      allOf:
        - {required: [data], properties: {data: {$ref: '#/components/schemas/NarrowData'}}}
    NarrowData:
      required: [planned_rate]
      properties: {planned_rate: {type: [integer, 'null']}}
"""

REV = """
components:
  messages:
    KeptAsIs: {payload: {$ref: '#/components/schemas/KeptEvent'}}
    FieldRenamed: {payload: {$ref: '#/components/schemas/RenamedEvent'}}
    TypeNarrowed: {payload: {$ref: '#/components/schemas/NarrowEvent'}}
    AddedInRev: {payload: {$ref: '#/components/schemas/KeptEvent'}}  # addition: fine
  schemas:
    KeptEvent:
      allOf:
        - {required: [data], properties: {data: {$ref: '#/components/schemas/KeptData'}}}
    KeptData:  # note widens [string,'null'] -> [string,'null',integer]: fine
      required: [site_code, note]
      properties:
        site_code: {type: string}
        note: {type: [string, 'null', integer]}
        added_optional: {type: string}
    RenamedEvent:
      allOf:
        - {required: [data], properties: {data: {$ref: '#/components/schemas/RenamedData'}}}
    RenamedData:  # site_code -> siteCode: the old required name is gone
      required: [siteCode]
      properties: {siteCode: {type: string}}
    NarrowEvent:
      allOf:
        - {required: [data], properties: {data: {$ref: '#/components/schemas/NarrowData'}}}
    NarrowData:  # [integer,'null'] -> integer
      required: [planned_rate]
      properties: {planned_rate: {type: integer}}
"""

def self_check():
    expected = [  # in sorted() order: findings start with the message name
        "FieldRenamed.data: required field removed or renamed: site_code",
        "TypeNarrowed.data.planned_rate: type narrowed: ['integer', 'null'] -> ['integer']",
        "message removed: GoneInRev",
    ]
    got = sorted(breaking(yaml.safe_load(BASE), yaml.safe_load(REV)))
    if got != expected:
        print("self-check FAILED")
        for line in got:
            print("  got: ", line)
        for line in expected:
            print("  want:", line)
        return 1
    print(f"asyncapi_breaking self-check OK: {len(expected)}/{len(expected)} findings detected")
    return 0


def main(argv):
    if argv == ["--self-check"]:
        return self_check()
    if len(argv) != 2:
        print(__doc__)
        return 2
    findings = sorted(breaking(load(argv[0]), load(argv[1])))
    for line in findings:
        print(f"::error::{line}")
    if findings:
        print(
            f"\n{len(findings)} breaking AsyncAPI change(s). The contract is additive-only "
            "(ADR-0026): ship a new .v2 message type and dataschema version instead. "
            "Pinned copies of this spec: warehouse-planning, wes-work-planning."
        )
        return 1
    print("no breaking AsyncAPI changes")
    return 0


def load(path):
    with open(path, encoding="utf-8") as fh:
        return yaml.safe_load(fh) or {}


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
