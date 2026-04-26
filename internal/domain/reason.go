package domain

type ReasonCode uint32

const (
	ValidStorageProof ReasonCode = 0

	IPNotFound          ReasonCode = 101
	NotFound            ReasonCode = 102
	UnavailableProvider ReasonCode = 103
	CantCreatePeer      ReasonCode = 104
	UnknownPeer         ReasonCode = 105

	PingFailed        ReasonCode = 201
	InvalidBagID      ReasonCode = 202
	FailedInitialPing ReasonCode = 203

	GetInfoFailed ReasonCode = 301
	InvalidHeader ReasonCode = 302

	CantGetPiece     ReasonCode = 401
	CantParseBoC     ReasonCode = 402
	ProofCheckFailed ReasonCode = 403
)
