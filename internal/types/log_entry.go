package types

import (
	pb "paxos-banking/proto"
)

type LogEntry struct {
	Ballot     *Ballot
	SeqNum     int32
	Request    *pb.TransactionRequest
	IsNoOp     bool
	Status     string // "A"=Accepted, "C"=Committed, "E"=Executed
	AcceptedBy map[int32]bool
	Phase      string        // 2PC phase: ""=normal, "P"=Prepare, "C"=Commit, "A"=Abort
	Result     pb.ResultType // Transaction result
}

func NewLogEntry(ballot *Ballot, seqNum int32, req *pb.TransactionRequest, isNoOp bool) *LogEntry {
	return &LogEntry{
		Ballot:     ballot,
		SeqNum:     seqNum,
		Request:    req,
		IsNoOp:     isNoOp,
		Status:     "A",
		AcceptedBy: make(map[int32]bool),
		Phase:      "", // Default: normal transaction
		Result:     pb.ResultType_SUCCESS,
	}
}

func NewLogEntryWithPhase(ballot *Ballot, seqNum int32, req *pb.TransactionRequest, isNoOp bool, phase string) *LogEntry {
	return &LogEntry{
		Ballot:     ballot,
		SeqNum:     seqNum,
		Request:    req,
		IsNoOp:     isNoOp,
		Status:     "A",
		AcceptedBy: make(map[int32]bool),
		Phase:      phase, // 2PC phase marker
		Result:     pb.ResultType_SUCCESS,
	}
}
