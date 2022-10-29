package proxy

import (
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/version"
)

// RequestInfo contains all the information for sending
// the abci.RequestInfo message during handshake with the app.
// It contains only compile-time version information.
var RequestInfo = abci.RequestInfo{
	Version:      version.TMCoreSemVer,
	BlockVersion: version.BlockProtocol,
	P2PVersion:   version.P2PProtocol,
}

// Added by Yi
func RequestInfoWithChainID(chainID string) abci.RequestInfo {
	return abci.RequestInfo {
		Version:      version.TMCoreSemVer,
		BlockVersion: version.BlockProtocol,
		P2PVersion:   version.P2PProtocol,
		ChainID:      chainID,
	}
}
