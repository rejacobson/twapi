package browser

import (
	"bytes"
	"io"
	"math"
	"net"
	"sync"
	"time"
)

// RequestToken writes the payload to w
func RequestToken(w io.Writer) (err error) {
	tokenReq := NewTokenRequestPacket()
	n, err := w.Write(tokenReq)
	if err != nil {
		return
	} else if n != len(tokenReq) {
		err = ErrInvalidWrite
	}

	return
}

// ReceiveToken reads the token payload from the reader r
func ReceiveToken(r io.Reader) (response []byte, err error) {
	response = make([]byte, tokenResponseSize)
	read, err := r.Read(response)
	if err != nil {
		return nil, err
	}

	if read != tokenResponseSize {
		err = ErrInvalidResponseMessage
	}

	return response[:read], err
}

// FetchToken tries to fetch a token from the server for a specific duration at most. a timeout below 35 ms will be set to 35 ms
func FetchToken(rwd ReadWriteDeadliner, timeout time.Duration) (response []byte, err error) {
	if timeout < minTimeout {
		timeout = minTimeout
	}

	begin := time.Now()
	timeLeft := timeout
	currentTimeout := minTimeout
	writeBurst := 1.0

	for {
		timeLeft = timeout - time.Since(begin)
		rwd.SetReadDeadline(time.Now().Add(currentTimeout))

		if timeLeft <= 0 {
			// early return, because timed out
			err = ErrTimeout
			return
		}

		// send multiple requests
		for i := 0.0; i < writeBurst; i += 1.0 {
			err = RequestToken(rwd)
			if err != nil {
				return
			}
		}

		// wait for response
		response, err = ReceiveToken(rwd)
		if err == nil {
			return
		}

		// increase time & request burst
		timeLeft = timeout - time.Since(begin)
		if timeLeft <= currentTimeout {
			currentTimeout = timeLeft
		} else {
			currentTimeout *= 2
		}
		writeBurst *= 1.2
	}
}

// Request writes the payload into w.
// w can be a buffer or a udp connection
// packet can be one of:
//		"serverlist"
//		"servercount"
//		"serverinfo"
func Request(packet string, token Token, w io.Writer) (err error) {
	var payload []byte
	switch packet {
	case "serverlist":
		payload, err = NewServerListRequestPacket(token)
	case "servercount":
		payload, err = NewServerCountRequestPacket(token)
	case "serverinfo":
		payload, err = NewServerInfoRequestPacket(token)
	}
	if err != nil {
		return
	}

	n, err := w.Write(payload)
	if err != nil {
		return
	} else if n != len(payload) {
		err = ErrInvalidWrite
	}
	return
}

// Receive reads the response message and evaluates its validity.
// If the message is not valid it is still returned.
func Receive(packet string, r io.Reader) (response []byte, err error) {
	response = make([]byte, maxBufferSize)

	read, err := r.Read(response)
	if err != nil {
		return nil, err
	}

	response = response[:read]

	if read == 0 {
		return response, ErrInvalidResponseMessage
	}

	match, err := MatchResponse(response)
	if err != nil {
		return nil, err
	}

	if match != packet {
		err = ErrRequestResponseMismatch
	}
	return response, err
}

// FetchWithToken is the same as Fetch, but it retries fetching data for a specific time.
func FetchWithToken(packet string, token Token, rwd ReadWriteDeadliner, timeout time.Duration) (response []byte, err error) {
	if timeout < minTimeout {
		timeout = minTimeout
	}

	begin := time.Now()
	timeLeft := timeout
	currentTimeout := minTimeout
	writeBurst := 1

	for {
		timeLeft = timeout - time.Since(begin)
		rwd.SetReadDeadline(time.Now().Add(currentTimeout))

		if timeLeft <= 0 {
			// early return, because timed out
			err = ErrTimeout
			return
		}

		// send multiple requests
		for i := 0; i < writeBurst; i++ {
			err = Request(packet, token, rwd)
			if err != nil {
				return
			}
		}

		// wait for response
		response, err = Receive(packet, rwd)
		if err == nil {
			return
		}

		// increase time & request burst
		timeLeft = timeout - time.Since(begin)
		if timeLeft <= currentTimeout {
			currentTimeout = timeLeft
		} else {
			currentTimeout *= 2
		}
		writeBurst *= 2
	}
}

// MatchResponse matches a respnse to a specific string
// "", ErrInvalidResponseMessage -> if response message contains invalid data
// "", ErrInvalidHeaderLength -> if response message is too short
// "token" - token response
// "serverlist" - server list response
// "servercount" - server count response
// "serverinfo" - server info response
func MatchResponse(responseMessage []byte) (string, error) {
	if len(responseMessage) < minPrefixLength {
		return "", ErrInvalidHeaderLength
	}

	if len(responseMessage) == tokenResponseSize {
		return "token", nil
	} else if bytes.Equal(sendServerListRaw, responseMessage[tokenPrefixSize:tokenPrefixSize+len(sendServerListRaw)]) {
		return "serverlist", nil
	} else if bytes.Equal(sendServerCountRaw, responseMessage[tokenPrefixSize:tokenPrefixSize+len(sendServerCountRaw)]) {
		return "servercount", nil
	} else if bytes.Equal(sendInfoRaw, responseMessage[tokenPrefixSize:tokenPrefixSize+len(sendInfoRaw)]) {
		return "serverinfo", nil
	}
	return "", ErrInvalidResponseMessage
}

// Fetch sends the token, retrieves the response and sends the follow up packet request in order to receive the data response.
func Fetch(packet string, rwd ReadWriteDeadliner, timeout time.Duration) (response []byte, err error) {
	begin := time.Now()
	resp, err := FetchToken(rwd, timeout)
	if err != nil {
		return
	}
	token, err := ParseToken(resp)
	if err != nil {
		return
	}
	timeLeft := timeout - time.Since(begin)
	resp, err = FetchWithToken(packet, token, rwd, timeLeft)
	if err != nil {
		return
	}

	response = resp
	return
}

// ServerInfos is a wrapper for ServerInfosWithTimeouts with prefedined parameters that have been deemed to work
// with a rather low packet loss, but still being rather small.
func ServerInfos() (infos []ServerInfo) {
	return ServerInfosWithTimeouts(TimeoutMasterServers, TimeoutServers)
}

// GetServerInfoWithTimeout fetches the server info from the passed address
// if the timeout is less than 60ms the default if 60ms is used.
// 60ms has been tested to be the lowest sane response time to get the server info.
func GetServerInfoWithTimeout(ip string, port int, timeout time.Duration) (ServerInfo, error) {
	info := ServerInfo{}

	ipAddr := net.ParseIP(ip)

	if ipAddr == nil {
		return info, ErrInvalidIP
	}

	if port < 0 || math.MaxUint16 < port {
		return info, ErrInvalidPort
	}

	if timeout < minTimeout {
		timeout = minTimeout
	}

	srv := &net.UDPAddr{
		IP:   ipAddr,
		Port: port,
	}

	conn, err := net.DialUDP("udp", nil, srv)
	if err != nil {
		return info, err
	}
	defer conn.Close()

	// increase buffers for writing and reading
	conn.SetReadBuffer(maxBufferSize)
	conn.SetWriteBuffer(int(maxBufferSize * timeout.Seconds()))

	resp, err := Fetch("serverinfo", conn, timeout)
	if err != nil {
		return info, err
	}

	info, err = ParseServerInfo(resp, srv.String())
	if err != nil {
		return info, err
	}

	return info, nil
}

// GetServerInfo fetches the server info of a given ip and port.
// it timeouts after about 16 seconds. If a smaller timeout and response time is needed, please use
// GetServerInfoWithTimeout() instead. 
func GetServerInfo(ip string, port int) (ServerInfo, error) {
	return GetServerInfoWithTimeout(ip, port, TimeoutServers)
}

// ServerInfosWithTimeouts retrieves the full serverlist with all of the server's infos from the masterservers as well as the individual servers
// it is possible to set the masterserver and the per server timeouts manually.
func ServerInfosWithTimeouts(timeoutMasterServer, timeoutServer time.Duration) (infos []ServerInfo) {
	cm := NewConcurrentMap(512)

	var wg sync.WaitGroup
	wg.Add(len(MasterServerAddresses))

	for _, ms := range MasterServerAddresses {
		ms := ms
		go fetchServersFromMasterServerAddress(ms, timeoutMasterServer, timeoutServer, &cm, &wg)
	}

	wg.Wait()

	infos = cm.Values()
	return
}

func fetchServersFromMasterServerAddress(ms *net.UDPAddr, timeoutMasterServer, timeoutServer time.Duration, cm *ConcurrentMap, wg *sync.WaitGroup) {
	defer wg.Done()

	conn, err := net.DialUDP("udp", nil, ms)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetWriteBuffer(maxBufferSize * maxChunks)

	resp, err := Fetch("serverlist", conn, timeoutMasterServer)
	if err != nil {
		return
	}

	servers, err := ParseServerList(resp)
	if err != nil {
		return
	}

	var infoWaiter sync.WaitGroup

	infoWaiter.Add(len(servers))
	for _, s := range servers {
		s := s
		go fetchServerInfoFromServerAddress(s, timeoutServer, cm, &infoWaiter)
	}
	infoWaiter.Wait()
}

func fetchServerInfoFromServerAddress(srv *net.UDPAddr, timeout time.Duration, cm *ConcurrentMap, wg *sync.WaitGroup) {
	defer wg.Done()

	conn, err := net.DialUDP("udp", nil, srv)
	if err != nil {
		return
	}
	defer conn.Close()

	// increase buffers for writing and reading
	conn.SetReadBuffer(maxBufferSize)
	conn.SetWriteBuffer(int(maxBufferSize * timeout.Seconds()))

	resp, err := Fetch("serverinfo", conn, timeout)
	if err != nil {
		return
	}

	info, err := ParseServerInfo(resp, srv.String())
	if err != nil {
		return
	}

	cm.Add(info, 0)
}
