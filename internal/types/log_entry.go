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
}

func NewLogEntry(ballot *Ballot, seqNum int32, req *pb.TransactionRequest, isNoOp bool) *LogEntry {
	return &LogEntry{
		Ballot:     ballot,
		SeqNum:     seqNum,
		Request:    req,
		IsNoOp:     isNoOp,
		Status:     "A",
		AcceptedBy: make(map[int32]bool),
	}
}
