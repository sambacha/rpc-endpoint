/*
Request represents an incoming client request
*/
package server

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"
)

// MetaMask keeps re-sending tx, bombarding the system with eth_sendRawTransaction calls. If this happens, we prevent
// the tx from being forwarded to the TxManager, and force MetaMask to return an error (using eth_getTransactionCount).
var blacklistedRawTx = make(map[string]time.Time) // key is the rawTxHex, value is time added
var mmBlacklistedAccountAndNonce = make(map[string]*mmNonceHelper)

type mmNonceHelper struct {
	Nonce    uint64
	NumTries uint64
}

// RPC request for a single client JSON-RPC request
type RpcRequest struct {
	respw *http.ResponseWriter
	req   *http.Request

	uid             string
	timeStarted     time.Time
	defaultProxyUrl string
	txManagerUrl    string

	// extracted during request lifecycle:
	body     []byte
	jsonReq  *JsonRpcRequest
	ip       string
	rawTxHex string
	tx       *types.Transaction
	txFrom   string

	// response flags
	respHeaderContentTypeWritten bool
	respHeaderStatusCodeWritten  bool
	respBodyWritten              bool
}

func NewRpcRequest(respw *http.ResponseWriter, req *http.Request, proxyUrl string, txManagerUrl string) *RpcRequest {
	return &RpcRequest{
		respw:           respw,
		req:             req,
		uid:             uuid.New().String(),
		timeStarted:     time.Now(),
		defaultProxyUrl: proxyUrl,
		txManagerUrl:    txManagerUrl,
	}
}

func (r *RpcRequest) log(format string, v ...interface{}) {
	prefix := fmt.Sprintf("[%s] ", r.uid)
	log.Printf(prefix+format, v...)
}

func (r *RpcRequest) logError(format string, v ...interface{}) {
	prefix := fmt.Sprintf("[%s] ERROR: ", r.uid)
	log.Printf(prefix+format, v...)
}

func (r *RpcRequest) writeHeaderStatus(statusCode int) {
	if r.respHeaderStatusCodeWritten {
		return
	}
	(*r.respw).WriteHeader(http.StatusUnauthorized)
	r.respHeaderStatusCodeWritten = true
}

func (r *RpcRequest) writeHeaderContentType(contentType string) {
	if r.respHeaderContentTypeWritten {
		return
	}
	(*r.respw).Header().Set("Content-Type", contentType)
	r.respHeaderContentTypeWritten = true
}

func (r *RpcRequest) writeHeaderContentTypeJson() {
	r.writeHeaderContentType("application/json")
}

func (r *RpcRequest) process() {
	var err error

	// At end of request, log the time it needed
	defer func() {
		timeRequestNeeded := time.Since(r.timeStarted)
		r.log("request took %.6f sec", timeRequestNeeded.Seconds())
	}()

	r.ip = GetIP(r.req)
	r.log("POST request from ip: %s - goroutines: %d", r.ip, runtime.NumGoroutine())

	if IsBlacklisted(r.ip) {
		r.log("Blocked: IP=%s", r.ip)
		r.writeHeaderStatus(http.StatusUnauthorized)
		return
	}

	// If users specify a proxy url in their rpc endpoint they can have their requests proxied to that endpoint instead of Infura
	// e.g. https://rpc.flashbots.net?url=http://RPC-ENDPOINT.COM
	customProxyUrl, ok := r.req.URL.Query()["url"]
	if ok && len(customProxyUrl[0]) > 1 {
		r.defaultProxyUrl = customProxyUrl[0]
		r.log("Using custom url: %s", r.defaultProxyUrl)
	}

	// Decode request JSON RPC
	defer r.req.Body.Close()
	r.body, err = ioutil.ReadAll(r.req.Body)
	if err != nil {
		r.logError("failed to read request body: %v", err)
		r.writeHeaderStatus(http.StatusBadRequest)
		return
	}

	// Parse JSON RPC
	if err = json.Unmarshal(r.body, &r.jsonReq); err != nil {
		r.logError("failed to parse JSON RPC request: %v", err)
		r.writeHeaderStatus(http.StatusBadRequest)
		return
	}

	r.log("JSON-RPC method: %s ip: %s", r.jsonReq.Method, r.ip)
	// if strings.Contains(os.Getenv("RPC_LOG_PARAMS_FOR"), r.jsonReq.Method) {
	// 	r.log("rpcreq method: %s args: %s", r.jsonReq.Method, r.jsonReq.Params)
	// }

	if r.jsonReq.Method == "eth_sendRawTransaction" {
		r.handle_sendRawTransaction()

	} else {
		// Normal proxy mode. Check for intercepts
		if r.jsonReq.Method == "eth_getTransactionCount" && r.intercept_mm_eth_getTransactionCount() { // intercept if MM needs to show an error to user
			return
		} else if r.jsonReq.Method == "eth_call" && r.intercept_eth_call_to_FlashRPC_Contract() { // intercept if Flashbots isRPC contract
			return
		} else if r.jsonReq.Method == "net_version" {
			r.writeRpcResult("1")
			return
		}

		// Proxy the request to a node
		readJsonRpcSuccess, proxyHttpStatus, jsonResp := r.proxyRequestRead(r.defaultProxyUrl)

		// Write the response to user
		if readJsonRpcSuccess {
			r.writeHeaderStatus(proxyHttpStatus)
			r._writeRpcResponse(jsonResp)
			r.log("Proxy to node successful: %s", r.jsonReq.Method)
		} else {
			r.writeHeaderStatus(http.StatusInternalServerError)
			r.log("Proxy to node failed: %s", r.jsonReq.Method)
		}
	}
}

func (r *RpcRequest) handle_sendRawTransaction() {
	var err error

	// JSON-RPC sanity checks
	if len(r.jsonReq.Params) < 1 {
		r.logError("no params for eth_sendRawTransaction")
		r.writeHeaderStatus(http.StatusBadRequest)
		return
	}

	r.rawTxHex = r.jsonReq.Params[0].(string)
	if len(r.rawTxHex) < 2 {
		r.logError("invalid raw transaction (wrong length)")
		r.writeHeaderStatus(http.StatusBadRequest)
		return
	}

	r.log("rawTx: %s", r.rawTxHex)

	if _, isBlacklistedTx := blacklistedRawTx[r.rawTxHex]; isBlacklistedTx {
		r.log("rawTx blocked because bundle failed too many times")
		r.writeRpcError("rawTx blocked because bundle failed too many times")
		return
	}

	r.tx, err = GetTx(r.rawTxHex)
	if err != nil {
		r.logError("Error getting transaction object")
		r.writeHeaderStatus(http.StatusBadRequest)
		return
	}

	r.txFrom, err = GetSenderFromRawTx(r.tx)
	if err != nil {
		r.logError("couldn't get address from rawTx: %v", err)
		r.writeHeaderStatus(http.StatusBadRequest)
		return
	}

	if isOnOFACList(r.txFrom) {
		r.log("BLOCKED TX FROM OFAC SANCTIONED ADDRESS")
		r.writeHeaderStatus(http.StatusUnauthorized)
		return
	}

	needsProtection, err := r.doesTxNeedFrontrunningProtection(r.tx)
	if err != nil {
		r.logError("failed to evaluate transaction: %v", err)
		r.writeHeaderStatus(http.StatusBadRequest)
		return
	}

	target := "mempool"
	url := r.defaultProxyUrl
	if needsProtection {
		target = "Flashbots"
		url = r.txManagerUrl
	}

	// Proxy now!
	readJsonRpcSuccess, proxyHttpStatus, jsonResp := r.proxyRequestRead(url)

	// Log after proxying
	if !readJsonRpcSuccess {
		r.log("Proxy to %s failed: eth_sendRawTransaction", target)
		r.writeHeaderStatus(http.StatusInternalServerError)
		return
	}

	// Write JSON-RPC response now
	r.writeHeaderContentTypeJson()
	r.writeHeaderStatus(proxyHttpStatus)
	if jsonResp.Error != nil {
		r.log("Proxy to %s successful: eth_sendRawTransaction (with error in response)", target)

		// write the original response to the user
		r._writeRpcResponse(jsonResp)
		return
	} else {
		// TxManager returns bundle hash, but this call needs to return tx hash
		txHash := r.tx.Hash().Hex()
		r.writeRpcResult(txHash)
		r.log("Proxy to %s successful: eth_sendRawTransaction - tx-hash: %s", target, txHash)
		return
	}
}

// Proxies the incoming request to the target URL, and tries to parse JSON-RPC response (and check for specific)
func (r *RpcRequest) proxyRequestRead(proxyUrl string) (readJsonRpsResponseSuccess bool, httpStatusCode int, jsonResp *JsonRpcResponse) {
	timeProxyStart := time.Now() // for measuring execution time
	r.log("proxyRequest to: %s", proxyUrl)

	// Proxy request
	proxyResp, err := ProxyRequest(proxyUrl, r.body)

	// Afterwards, check time and result
	timeProxyNeeded := time.Since(timeProxyStart)
	r.log("proxy response %d after %.6f: %v", proxyResp.StatusCode, timeProxyNeeded.Seconds(), proxyResp)
	if err != nil {
		r.logError("failed to make proxy request: %v", err)
		return false, proxyResp.StatusCode, jsonResp
	}

	// Read body
	defer proxyResp.Body.Close()
	proxyRespBody, err := ioutil.ReadAll(proxyResp.Body)
	if err != nil {
		r.logError("failed to decode proxy request body: %v", err)
		return false, proxyResp.StatusCode, jsonResp
	}

	// Unmarshall JSON-RPC response and check for error inside
	jsonRpcResp := new(JsonRpcResponse)
	if err := json.Unmarshal(proxyRespBody, jsonRpcResp); err != nil {
		r.logError("failed decoding proxy json-rpc response: %v", err)
		return false, proxyResp.StatusCode, jsonResp
	}

	// If JSON-RPC had an error response, parse but still pass back to user
	if jsonRpcResp.Error != nil {
		r.handleProxyError(jsonRpcResp.Error)
	}

	return true, proxyResp.StatusCode, jsonRpcResp
}

func (r *RpcRequest) handleProxyError(rpcError *JsonRpcError) {
	r.log("proxy response json-rpc error: %s", rpcError.Error())

	if rpcError.Message == "Bundle submitted has already failed too many times" {
		blacklistedRawTx[r.rawTxHex] = time.Now()
		r.log("rawTx added to blocklist. entries: %d", len(blacklistedRawTx))

		// Cleanup old rawTx blacklist entries
		for key, entry := range blacklistedRawTx {
			if time.Since(entry) > 4*time.Hour {
				delete(blacklistedRawTx, key)
			}
		}

		// To prepare for MM retrying the transactions, we get the txCount and then return it +1 for next four tries
		nonce, err := eth_getTransactionCount(r.defaultProxyUrl, r.txFrom)
		if err != nil {
			r.logError("failed getting nonce: %s", err)
			return
		}
		// fmt.Println("NONCE", nonce, "for", r.txFrom)
		mmBlacklistedAccountAndNonce[strings.ToLower(r.txFrom)] = &mmNonceHelper{
			Nonce: nonce,
		}
	}
}

// Check if a request needs frontrunning protection. There are many transactions that don't need frontrunning protection,
// for example simple ERC20 transfers.
func (r *RpcRequest) doesTxNeedFrontrunningProtection(tx *types.Transaction) (bool, error) {
	gas := tx.Gas()
	r.log("[protect-check] gas: %v", gas)

	// Flashbots Relay will reject anything less than 42000 gas, so we just send those to the mempool
	// Anyway things with that low of gas probably don't need frontrunning protection regardless
	if gas < 42000 {
		return false, nil
	}

	data := hex.EncodeToString(tx.Data())
	r.log("[protect-check] data: %v", data)
	if len(data) == 0 {
		r.log("[protect-check] Data had a length of 0, but a gas greater than 21000. Sending cancellation tx to mempool.")
		return false, nil
	}

	if isOnFunctionWhiteList(data[0:8]) {
		return false, nil // function being called is on our whitelist and no protection needed
	} else {
		return true, nil // needs protection if not on whitelist
	}
}

func (r *RpcRequest) writeRpcError(msg string) {
	res := JsonRpcResponse{
		Id:      r.jsonReq.Id,
		Version: "2.0",
		Error: &JsonRpcError{
			Code:    -32603,
			Message: msg,
		},
	}
	r._writeRpcResponse(&res)
}

func (r *RpcRequest) writeRpcResult(result interface{}) {
	res := JsonRpcResponse{
		Id:      r.jsonReq.Id,
		Version: "2.0",
		Result:  result,
	}
	r._writeRpcResponse(&res)
}

func (r *RpcRequest) _writeRpcResponse(res *JsonRpcResponse) {
	if r.respBodyWritten {
		r.logError("_writeRpcResponse: response already written")
		return
	}

	r.writeHeaderContentTypeJson()     // set content type to json, if not yet set
	r.writeHeaderStatus(http.StatusOK) // set status header to 200, if not yet set

	if err := json.NewEncoder(*r.respw).Encode(res); err != nil {
		r.logError("failed writing rpc response: %v", err)
		r.writeHeaderStatus(http.StatusInternalServerError)
	}

	r.respBodyWritten = true
}
