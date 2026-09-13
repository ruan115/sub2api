# Portunex recovery namespace

This package creates deterministic namespaces for Portunex-owned Redis-style
keys, channels, locks, and opaque object identifiers.  It is deliberately a
pure value formatter: it does not open a Redis, database, or network
connection.

`New(instance)` creates an isolated recovery space.  Its outputs begin with
`portunex:v1:<instance>` and always include a resource-type component:

```text
Key(family, id)     -> portunex:v1:<instance>:key:<family>:<id>
Channel(topic)      -> portunex:v1:<instance>:channel:<topic>
Lock(name)          -> portunex:v1:<instance>:lock:<name>
ObjectID(kind, id)  -> portunex:v1:<instance>:object:<kind>:<id>
```

The instance is a recovery deployment identifier, not a secret.  It keeps a
recovery deployment separate from existing backend namespaces.  Every caller
must receive a `Space`; no package-level default is provided.

All components are non-empty ASCII identifiers.  They must start with an
alphanumeric character and may then contain only alphanumerics, `.`, `_`, or
`-`.  This rejects `:`, whitespace, controls, and Unicode so no component can
inject or collide with the `:` delimiter.  Values are never parsed or
normalized; notably, `"01"` and `"1"` remain distinct IDs.  Invalid input
returns an error that identifies the argument class but deliberately does not
echo the supplied value.

This naming discipline is stronger than choosing a different Redis logical DB:
Pub/Sub channels and lock/object identifiers remain explicitly namespaced too.
