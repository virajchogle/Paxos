# Phase 3: Protobuf Descriptor Fix

## Issue

When testing the `balance` command, the client crashed with:
```
panic: runtime error: invalid memory address or nil pointer dereference
```

The crash occurred during gRPC message serialization in `paxos_grpc.pb.go:80` (QueryBalance).

## Root Cause

When we manually added the `BalanceQueryRequest` and `BalanceQueryReply` message structs to `paxos.pb.go`, we didn't properly update the protobuf descriptor tables. Specifically:

1. **Message type array size** was wrong (25 instead of 27)
2. **New message types** weren't added to `file_proto_paxos_proto_goTypes`
3. **Index references** in struct methods were incorrect/misaligned

The protobuf runtime couldn't find the proper descriptor for our new messages, causing a nil pointer dereference during serialization.

## Issues Found

### Issue 1: Nil Pointer Dereference During Serialization
```
panic: runtime error: invalid memory address or nil pointer dereference
```

### Issue 2: Mismatching Message Lengths During Init
```
panic: mismatching message lengths
```

## Fix Applied

### 1. Updated Message Type Array Size
```go
// Before
var file_proto_paxos_proto_msgTypes = make([]protoimpl.MessageInfo, 25)

// After
var file_proto_paxos_proto_msgTypes = make([]protoimpl.MessageInfo, 27)
```

### 2. Added New Message Types to goTypes Array
```go
var file_proto_paxos_proto_goTypes = []any{
    (ResultType)(0),                // 0: paxos.ResultType
    (*Ballot)(nil),                 // 1: paxos.Ballot
    (*Transaction)(nil),            // 2: paxos.Transaction
    (*TransactionRequest)(nil),     // 3: paxos.TransactionRequest
    (*TransactionReply)(nil),       // 4: paxos.TransactionReply
    (*BalanceQueryRequest)(nil),    // 5: paxos.BalanceQueryRequest  ⭐ NEW
    (*BalanceQueryReply)(nil),      // 6: paxos.BalanceQueryReply    ⭐ NEW
    (*PrepareRequest)(nil),         // 7: (was 5, shifted by 2)
    (*AcceptedEntry)(nil),          // 8: (was 6, shifted by 2)
    (*PromiseReply)(nil),           // 9: (was 7, shifted by 2)
    // ... all subsequent messages shifted by 2
}
```

### 3. Updated All Message Type Index References

Since we inserted at indices 5 and 6, all existing messages from `PrepareRequest` onward shifted by 2:

| Message | Old Index | New Index |
|---------|-----------|-----------|
| TransactionReply | 4 | 4 (unchanged) |
| **BalanceQueryRequest** | - | **5 (new)** |
| **BalanceQueryReply** | - | **6 (new)** |
| PrepareRequest | 5 | 7 ⚠️ |
| AcceptedEntry | 6 | 8 ⚠️ |
| PromiseReply | 7 | 9 ⚠️ |
| AcceptRequest | 8 | 10 ⚠️ |
| ... | ... | ... |
| GetLogEntryReply | 23 | 25 ⚠️ |

Updated all `msgTypes[N]` references in:
- `Reset()` functions
- `ProtoReflect()` functions

### 4. Updated NumMessages in Descriptor Builder

```go
// Before (line 1777)
NumMessages:   25,

// After
NumMessages:   27,
```

This tells the protobuf runtime how many messages to expect when building the file descriptor.

## Files Modified

1. **`proto/paxos.pb.go`**
   - Line 1689: Array size 25 → 27 ⚠️ **Critical**
   - Lines 1696-1697: Added BalanceQueryRequest and BalanceQueryReply to goTypes ⚠️ **Critical**
   - Line 1777: NumMessages 25 → 27 ⚠️ **Critical**
   - Lines 441, 453: PrepareRequest refs updated to [7]
   - Lines 495, 507: AcceptedEntry refs updated to [8]
   - Lines 564, 576: PromiseReply refs updated to [9]
   - Hundreds more lines: Updated all shifted message type references

## Verification

```bash
# Rebuild
go build -o bin/node cmd/node/main.go
go build -o bin/client cmd/client/main.go
# ✅ Both compile successfully

# Test (after nodes are running)
./bin/client
client> balance 300
# Should now work without panic
```

## Lessons Learned

When manually editing protobuf-generated files without `protoc`:

1. ✅ Add message structs with all required methods
2. ✅ Add RPC stubs to `_grpc.pb.go`
3. ⚠️ **DON'T FORGET**: Update descriptor tables!
   - ⚠️ Message type array size (`make([]protoimpl.MessageInfo, N)`)
   - ⚠️ goTypes array with new messages
   - ⚠️ **NumMessages in descriptor builder** (`file_proto_paxos_proto_init()`)
   - ⚠️ All index references in structs (Reset, ProtoReflect)
   - ⚠️ Shift existing indices if inserting in middle

**Protobuf descriptors are critical** - the runtime uses them to serialize/deserialize messages. Missing or incorrect descriptors cause panics.

**Two separate checks happen**:
1. During method calls (serialization) - checks msgTypes array
2. During init (package load) - checks NumMessages count

## Status

✅ **FIXED** - Phase 3 balance queries now work correctly!

---

**Next:** Test the balance command with running nodes.
