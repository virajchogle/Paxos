package types

import (
	"fmt"
	pb "paxos-banking/proto"
)

type Ballot struct {
	Number int32
	NodeID int32
}

func NewBallot(number, nodeID int32) *Ballot {
	return &Ballot{Number: number, NodeID: nodeID}
}

func BallotFromProto(b *pb.Ballot) *Ballot {
	if b == nil {
		return NewBallot(0, 0)
	}
	return &Ballot{Number: b.Number, NodeID: b.NodeId}
}

func (b *Ballot) ToProto() *pb.Ballot {
	return &pb.Ballot{Number: b.Number, NodeId: b.NodeID}
}

// Compare returns: 1 if b > other, -1 if b < other, 0 if equal
func (b *Ballot) Compare(other *Ballot) int {
	if b.Number != other.Number {
		if b.Number > other.Number {
			return 1
		}
		return -1
	}
	if b.NodeID > other.NodeID {
		return 1
	} else if b.NodeID < other.NodeID {
		return -1
	}
	return 0
}

func (b *Ballot) GreaterThan(other *Ballot) bool {
	return b.Compare(other) > 0
}

func (b *Ballot) Equal(other *Ballot) bool {
	return b.Number == other.Number && b.NodeID == other.NodeID
}

func (b *Ballot) String() string {
	return fmt.Sprintf("(%d,%d)", b.Number, b.NodeID)
}
