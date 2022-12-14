package eth

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/pioplat/pioplat-core/common"
	"github.com/pioplat/pioplat-core/common/hexutil"
	"github.com/pioplat/pioplat-core/core"
	"github.com/pioplat/pioplat-core/core/types"
	"github.com/pioplat/pioplat-core/log"
	"github.com/pioplat/pioplat-core/p2p/netutil"
	"github.com/pioplat/pioplat-core/rlp"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	TransactionMsg = 0x01
	BlockMsg       = 0x02
	TokenKey       = "token"
	PeerKey        = "peer"
	Ratio0Key      = "ratio_replace"
	Ratio1Key      = "ratio_block"
	Ratio2Key      = "ratio_tx"
	TxEncodedKey   = "tx"
	ListenPort     = 9998 // for tcp & udp listen, ListenPort - 1 (9997) for gin engine listen
)

type BlockEnqueueFn func(string, *types.Block) error
type TxEnqueueFn func(string, []*types.Transaction, bool) error
type HasTransactionFn func(hash common.Hash) bool
type HasBlockFn func(hash common.Hash) bool
type SetRatiosFn func(replaceRatio, blockRatio, txRatio float64)

type PioplatServer struct {
	lock            *sync.Mutex
	running         bool
	ListenAddr      string
	tcpListener     *net.TCPListener
	udpConn         *net.UDPConn
	broadcastOp     chan []byte
	broadcastOpDone chan struct{}
	addPeerCh       chan *pioplatConn
	chain           *core.Blockchain
	txPool          *core.TxPool
	hashSent        map[common.Hash]struct{}
	blockEnqueueFn  BlockEnqueueFn
	txsEnqueueFn    TxEnqueueFn
	setRatiosFn     SetRatiosFn

	// for external rpc invoke
	ginEngine  *gin.Engine
	adminToken string

	// for send transaction
	handler *handler

	// for encryption
	aesKey cipher.Block
	aesIv  []byte
}

func CreatePioplatServer(chain *core.Blockchain, txPool *core.TxPool, listenAddr, adminToken, keyIvHex string) *PioplatServer {
	keyIv, err := hexutil.Decode(keyIvHex)
	if err != nil {
		log.Crit("decode aes key/iv failed", "reason", err)
	}
	aesKey, err := aes.NewCipher(keyIv[:aes.BlockSize])
	if err != nil {
		log.Crit("aes new cipher failed", "reason", err)
	}

	srv := &PioplatServer{
		lock:            new(sync.Mutex),
		running:         false,
		ListenAddr:      listenAddr,
		tcpListener:     nil,
		udpConn:         nil,
		broadcastOp:     make(chan []byte),
		broadcastOpDone: make(chan struct{}),
		addPeerCh:       make(chan *pioplatConn),
		chain:           chain,
		txPool:          txPool,
		hashSent:        make(map[common.Hash]struct{}),
		adminToken:      adminToken,
		aesKey:          aesKey,
		aesIv:           keyIv[aes.BlockSize:],
	}

	// http server for control
	engine := gin.New()
	engine.POST("/dial", srv.dialHandler)
	engine.POST("/ratios", srv.setPeriRatio)
	engine.POST("/sendtx", srv.sendtxHandler)
	engine.Use(gin.Logger())
	srv.ginEngine = engine

	return srv
}

func (srv *PioplatServer) SetEnqueueFn(blockFn BlockEnqueueFn, txsFn TxEnqueueFn) {
	srv.blockEnqueueFn = blockFn
	srv.txsEnqueueFn = txsFn
}

func (srv *PioplatServer) SetHandler(handler *handler) {
	srv.handler = handler
}

func (srv *PioplatServer) cleanHashSent() {
	for {
		time.Sleep(time.Minute)
		srv.lock.Lock()
		srv.hashSent = make(map[common.Hash]struct{})
		srv.lock.Unlock()
	}
}

func (srv *PioplatServer) Start() error {
	srv.lock.Lock()
	defer srv.lock.Unlock()
	if srv.running {
		return errors.New("pioplat server already running")
	}
	srv.running = true

	if err := srv.setupListening(); err != nil {
		return err
	}
	go srv.run()
	go srv.handleUdpMsgLoop()

	go func() {
		// port 9997
		if err := srv.ginEngine.Run(":" + fmt.Sprint(ListenPort-1)); err != nil {
			log.Crit("gin engine encounter error", "reason", err)
		}
	}()

	return nil
}

func (srv *PioplatServer) setupServerForSendingTxs() error {
	var (
		err         error
		tcpAddr     *net.TCPAddr
		tcpListener net.Listener
		conn        net.Conn
	)
	tcpAddr, err = net.ResolveTCPAddr("tcp", fmt.Sprint("0.0.0.0:", ListenPort-2))
	if err != nil {
		return err
	}
	tcpListener, err = net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return err
	}
	go func() {
		for {
			conn, err = tcpListener.Accept()
			if netutil.IsTemporaryError(err) {
				time.Sleep(time.Second)
			} else if err != nil {
				return
			} else {
				go srv.serveForSendingTxs(conn)
			}
		}
	}()
	return nil
}

func (srv *PioplatServer) serveForSendingTxs(conn net.Conn) {
	var (
		err error
		rdr = bufio.NewReader(conn)
		buf = make([]byte, 0x2000)
		num uint32
	)
	const (
		ApiTokenLength = 32
		InvalidTx      = "0001"
		SendSucc       = "0002"
	)
	err = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err != nil {
		log.Warn("serve for sending txs connection failed (SetDeadline)", "reason", err)
		return
	}

	_, err = io.ReadFull(rdr, buf[:ApiTokenLength])
	if err != nil {
		log.Warn("serve for sending txs connection failed (read api token)", "reason", err)
		_ = conn.Close()
		return
	}
	if bytes.Compare(buf[:ApiTokenLength], []byte(srv.adminToken)) != 0 {
		log.Warn("serve for sending txs connection failed (read api token)", "reason", "invalid token")
		_ = conn.Close()
		return
	}

	for {
		_, err = io.ReadFull(rdr, buf[:LengthSize])
		if err != nil {
			_ = conn.Close()
			return
		}
		num = binary.LittleEndian.Uint32(buf[:LengthSize])
		_, err = io.ReadFull(rdr, buf[:num])
		if err != nil {
			_ = conn.Close()
			return
		}
		tx := &types.Transaction{}
		err = rlp.DecodeBytes(buf[:num], tx)
		if err != nil {
			copy(buf, InvalidTx)
		} else {

			srv.handler.peers.lock.Lock()
			for _, p := range srv.handler.peers.peers {
				go p.SendTransactions(types.Transactions{tx})
			}
			srv.handler.peers.lock.Unlock()

			copy(buf, SendSucc)
		}
		_, err = conn.Write(buf)
		if err != nil {
			_ = conn.Close()
			return
		}
	}

}

func (srv *PioplatServer) setupListening() error {
	var (
		err     error
		tcpAddr *net.TCPAddr
		udpAddr *net.UDPAddr
	)
	tcpAddr, err = net.ResolveTCPAddr("tcp", srv.ListenAddr)
	if err != nil {
		return err
	}
	srv.tcpListener, err = net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return err
	}
	log.Info("pioplat server: TCP listener up", "addr", srv.tcpListener.Addr().String())

	udpAddr, err = net.ResolveUDPAddr("udp", srv.ListenAddr)
	if err != nil {
		return err
	}
	srv.udpConn, err = net.ListenUDP("udp", udpAddr)
	if err != nil {
		return err
	}

	go srv.listenLoop()
	return nil
}

func (srv *PioplatServer) listenLoop() {
	for {
		var (
			fd      net.Conn
			err     error
			lastLog time.Time
		)
		for {
			fd, err = srv.tcpListener.Accept()
			if netutil.IsTemporaryError(err) {
				if time.Since(lastLog) > 1*time.Second {
					log.Debug("Temporary read error", "err", err)
					lastLog = time.Now()
				}
				time.Sleep(time.Millisecond * 200)
				continue
			} else if err != nil {
				log.Debug("Read error", "err", err)
				return
			}
			break
		}

		srv.setupConn(fd)
	}
}

func (srv *PioplatServer) setupConn(fd net.Conn) {
	addrString := fd.RemoteAddr().String()
	udpAddrString := strings.Split(addrString, ":")[0] + ":" + fmt.Sprint(ListenPort)
	udpAddr, _ := net.ResolveUDPAddr("udp", udpAddrString)
	srv.addPeerCh <- &pioplatConn{
		tcpRW:            fd,
		addr:             addrString,
		udpAddr:          udpAddr,
		aesKey:           srv.aesKey,
		aesIv:            srv.aesIv,
		enqueueBlockFn:   srv.blockEnqueueFn,
		enqueueTxFn:      srv.txsEnqueueFn,
		hasTransactionFn: srv.txPool.Has,
		hasBlockFn:       srv.chain.HasBlockByHash,
	}
}

func (srv *PioplatServer) AddPeerAsync(addr string) {
	go func() {
		var (
			err        error
			fd         net.Conn
			retryCount int
		)

		fd, err = net.Dial("tcp", addr)
		for err != nil && retryCount <= 10 {
			log.Warn("pioplat add peer dial failed", "reason", err)
			time.Sleep(5 * time.Second)
			retryCount += 1
			fd, err = net.Dial("tcp", addr)
		}

		if err != nil {
			log.Warn("pioplat retry 11 times still failed", "reason", err)
			return
		}

		srv.setupConn(fd)
	}()
}

func (srv *PioplatServer) BroadcastBlock(block *types.Block) {
	srv.lock.Lock()
	if _, ok := srv.hashSent[block.Hash()]; ok {
		srv.lock.Unlock()
		return
	} else {
		srv.hashSent[block.Hash()] = struct{}{}
	}
	srv.lock.Unlock()

	data, err := newBlockMsg(block)
	if err != nil {
		return
	}
	go func() {
		srv.broadcastOp <- data
	}()
}

func (srv *PioplatServer) BroadcastTransaction(tx *types.Transaction) {
	srv.lock.Lock()
	defer srv.lock.Unlock()
	if _, ok := srv.hashSent[tx.Hash()]; !ok {
		srv.hashSent[tx.Hash()] = struct{}{}

		data, err := newTransactionMsg(tx)
		if err != nil {
			return
		}
		go func() {
			srv.broadcastOp <- data
		}()
	}
}

func (srv *PioplatServer) run() {
	var (
		peers   = make(map[string]*pioplatConn)
		msgType byte
	)

	for {
		select {
		case c := <-srv.addPeerCh:
			peers[c.addr] = c
			go c.handleTcpMsgLoop()
			log.Info("pioplat accepts connected", "from", c.addr)
		case msg := <-srv.broadcastOp:
			msgType = msg[3]
			for addr, conn := range peers {
				go func(addr_ string, conn_ *pioplatConn) {
					var err error
					if msgType == TransactionMsg && len(msg) <= 1472 { // mss of UDP
						_, err = srv.udpConn.WriteToUDP(cryptMsg(msg, srv.aesKey, srv.aesIv), conn_.udpAddr)
						if err != nil {
							log.Warn("pioplat sent msg using udp failed", "reason", err, "target", addr_)
						}
					} else {
						_, err = conn_.tcpRW.Write(cryptMsg(msg, srv.aesKey, srv.aesIv))
						if err != nil {
							log.Warn("pioplat sent msg using tcp failed", "reason", err, "target", addr_)
							atomic.StoreInt32(&conn_.close, 1)
							delete(peers, addr_)
						}
					}
				}(addr, conn)
			}
		}
	}
}

func (srv *PioplatServer) handleUdpMsgLoop() {
	var (
		err         error
		recvN       int
		totalLength uint32
		recvBuf     = make([]byte, 0x1000)
		rdr         = bufio.NewReader(srv.udpConn)
		txHash      common.Hash
		checksum    uint32
	)

	const (
		HeaderSize = 40
		LengthSize = 4 // include 1 byte type field
		HashSize   = 32
		Crc32Size  = 4
	)

	for {
		_, err = io.ReadFull(rdr, recvBuf[:HeaderSize])
		if err != nil {
			log.Warn("pioplat udp read failed", "reason", err)
			continue
		}
		msgHeader := cryptMsg(recvBuf[:HeaderSize], srv.aesKey, srv.aesIv)

		// check header correctness
		checksum = crc32.ChecksumIEEE(msgHeader[:LengthSize+HashSize])
		if checksum != binary.LittleEndian.Uint32(msgHeader[LengthSize+HashSize:LengthSize+HashSize+Crc32Size]) {
			log.Warn("pioplat udp read failed (crc32 checksum header)")
			continue
		}

		if msgHeader[3] != TransactionMsg {
			log.Warn("pioplat udp read non transaction message")
			continue
		}
		msgHeader[3] = 0 // clear type byte

		totalLength = binary.LittleEndian.Uint32(msgHeader[:LengthSize])
		txHash.SetBytes(msgHeader[LengthSize : LengthSize+HashSize])

		recvN, err = io.ReadFull(rdr, recvBuf[HeaderSize:HeaderSize+totalLength])
		if err != nil {
			log.Warn("pioplat udp read failed", "reason", err)
			continue
		}

		msgHeaderBody := cryptMsg(recvBuf[:HeaderSize+totalLength], srv.aesKey, srv.aesIv)

		checksum = crc32.ChecksumIEEE(msgHeaderBody[HeaderSize : HeaderSize+recvN-Crc32Size])
		if checksum != binary.LittleEndian.Uint32(msgHeaderBody[HeaderSize+recvN-Crc32Size:HeaderSize+recvN]) {
			log.Warn("pioplat udp read failed (crc32 checksum rlp)")
			continue
		}

		if srv.txPool.Has(txHash) == false {
			tx := &types.Transaction{}
			err = rlp.DecodeBytes(msgHeaderBody[HeaderSize:HeaderSize+recvN-Crc32Size], tx)
			if err != nil {
				log.Warn("pioplat udp rlp decode tx failed", "reason", err)
				continue
			}
			// ":" to mark this transaction come from the other relay node
			_ = srv.txsEnqueueFn(":", types.Transactions{tx}, false)
		}
	}
}

type pioplatConn struct {
	tcpRW            net.Conn
	addr             string
	udpAddr          *net.UDPAddr
	close            int32
	aesKey           cipher.Block
	aesIv            []byte
	enqueueBlockFn   BlockEnqueueFn
	enqueueTxFn      TxEnqueueFn
	hasTransactionFn HasTransactionFn
	hasBlockFn       HasBlockFn
}

func (c *pioplatConn) handleTcpMsgLoop() {
	var (
		err         error
		recvN       int
		recvBuf     = make([]byte, 0x20000)
		rdr         = bufio.NewReader(c.tcpRW)
		hash        common.Hash
		msgType     byte
		checksum    uint32
		totalLength uint32
	)

	const (
		HeaderSize = 40
		LengthSize = 4 // include 1 byte type field
		HashSize   = 32
		Crc32Size  = 4
	)

	for atomic.LoadInt32(&c.close) != 1 {
		recvN, err = io.ReadFull(rdr, recvBuf[:HeaderSize])
		if err != nil {
			log.Warn("pioplat tcp read failed", "reason", err)
			break
		}
		msgHeader := cryptMsg(recvBuf[:HeaderSize], c.aesKey, c.aesIv)

		// check header correctness
		checksum = crc32.ChecksumIEEE(msgHeader[:LengthSize+HashSize])
		if checksum != binary.LittleEndian.Uint32(msgHeader[LengthSize+HashSize:LengthSize+HashSize+Crc32Size]) {
			log.Warn("pioplat tcp read failed (crc32 checksum header)")
			_, _ = c.tcpRW.Read(recvBuf) // drain it
			continue
		}
		msgType = msgHeader[3]
		msgHeader[3] = 0 // clear type byte
		totalLength = binary.LittleEndian.Uint32(msgHeader[:LengthSize])
		hash.SetBytes(msgHeader[LengthSize : LengthSize+HashSize])

		if int(totalLength) > len(recvBuf) {
			// apply for more cap
			headerCopy := make([]byte, HeaderSize)
			copy(headerCopy, recvBuf[:HeaderSize])
			recvBuf = make([]byte, HeaderSize+totalLength+1)
			copy(recvBuf[:HeaderSize], headerCopy)
		}

		recvN, err = io.ReadFull(rdr, recvBuf[HeaderSize:HeaderSize+totalLength])
		if err != nil {
			log.Warn("pioplat tcp read failed", "reason", err)
			continue
		}
		msgHeaderBody := cryptMsg(recvBuf[:HeaderSize+totalLength], c.aesKey, c.aesIv)

		checksum = crc32.ChecksumIEEE(msgHeaderBody[HeaderSize : HeaderSize+recvN-Crc32Size])
		if checksum != binary.LittleEndian.Uint32(msgHeaderBody[HeaderSize+recvN-Crc32Size:HeaderSize+recvN]) {
			log.Warn("pioplat tcp read failed (crc32 checksum rlp)")
			continue
		}

		switch msgType {
		case TransactionMsg:
			if c.hasTransactionFn(hash) == false {
				tx := &types.Transaction{}
				err = rlp.DecodeBytes(msgHeaderBody[HeaderSize:HeaderSize+recvN-Crc32Size], tx)
				if err != nil {
					log.Warn("pioplat tcp rlp decode tx failed", "reason", err)
					continue
				}
				// ":" to mark this transaction come from the other relay node
				_ = c.enqueueTxFn(":", types.Transactions{tx}, false)
			}
		case BlockMsg:
			if c.hasBlockFn(hash) == false {
				block := &types.Block{}
				err = rlp.DecodeBytes(msgHeaderBody[HeaderSize:HeaderSize+recvN-Crc32Size], block)
				if err != nil {
					log.Warn("pioplat tcp rlp decode block failed", "reason", err)
					continue
				}
				_ = c.enqueueBlockFn(c.addr, block) // todo
			}
		default:
			log.Warn("pioplat tcp read unknown type of message")
		}
	}

	c.tcpRW.Close()
	atomic.StoreInt32(&c.close, 1)
}

func cryptMsg(msg []byte, key cipher.Block, iv []byte) []byte {
	var ret = make([]byte, len(msg))
	stream := cipher.NewCTR(key, iv)
	stream.XORKeyStream(ret, msg)
	return ret
}

func newBlockMsg(block *types.Block) ([]byte, error) {
	// =======================================
	// 1 byte: type (block or transaction)
	// 3 bytes: length of the object encoded in rlp
	// 32 bytes: hash
	// 4 bytes: crc checksum of the header
	// ... bytes: block in rlp encoded
	// 4 bytes: crc checksum of the rlp
	const (
		HeaderSize = 40
		LengthSize = 4 // include 1 byte type field
		HashSize   = 32
		Crc32Size  = 4
	)
	data, err := rlp.EncodeToBytes(block)
	if err != nil {
		return nil, err
	}
	message := make([]byte, HeaderSize+len(data)+Crc32Size)
	binary.LittleEndian.PutUint32(message, uint32(len(data)+Crc32Size))
	message[3] = byte(BlockMsg)
	copy(message[LengthSize:], block.Hash().Bytes())

	checksum := crc32.ChecksumIEEE(message[:LengthSize+HashSize])
	binary.LittleEndian.PutUint32(message[LengthSize+HashSize:], checksum)

	copy(message[HeaderSize:], data)

	checksum = crc32.ChecksumIEEE(data)
	binary.LittleEndian.PutUint32(message[HeaderSize+len(data):], checksum)

	return message, nil
}

func newTransactionMsg(tx *types.Transaction) ([]byte, error) {
	// =======================================
	// 1 byte: type (block or transaction)
	// 3 bytes: length of the object encoded in rlp
	// 32 bytes: hash
	// 4 bytes: crc checksum of the header
	// ... bytes: block in rlp encoded
	// 4 bytes: crc checksum of the rlp
	const (
		HeaderSize = 40
		LengthSize = 4 // include 1 byte type field
		HashSize   = 32
		Crc32Size  = 4
	)
	data, err := rlp.EncodeToBytes(tx)
	if err != nil {
		return nil, err
	}
	message := make([]byte, HeaderSize+len(data)+Crc32Size)
	binary.LittleEndian.PutUint32(message, uint32(len(data)+Crc32Size))
	message[3] = byte(TransactionMsg)
	copy(message[LengthSize:], tx.Hash().Bytes())

	checksum := crc32.ChecksumIEEE(message[:LengthSize+HashSize])
	binary.LittleEndian.PutUint32(message[LengthSize+HashSize:], checksum)

	copy(message[HeaderSize:], data)
	checksum = crc32.ChecksumIEEE(data)
	binary.LittleEndian.PutUint32(message[HeaderSize+len(data):], checksum)
	return message, nil
}

var (
	AuthFailedErr      = gin.H{"error": "require correct token"}
	InvalidPeerErr     = gin.H{"error": "invalid peer addr"}
	InvalidBase64TxErr = gin.H{"error": "invalid transaction in based64"}
	InvalidRlpTxErr    = gin.H{"error": "invalid transaction in RLP"}
	InvalidFloatErr    = gin.H{"error": "invalid float number"}
	OpSuccessMsg       = gin.H{"msg": "operation success"}
)

// only admin can invoke this function
func (srv *PioplatServer) dialHandler(c *gin.Context) {
	var (
		token     string
		peer      string
		checkPass = false
	)
	token = c.PostForm(TokenKey)
	peer = c.PostForm(PeerKey)
	if token != srv.adminToken {
		c.JSON(http.StatusBadRequest, AuthFailedErr)
		return
	}

	for i := 0; i < len(peer); i++ {
		if peer[i] == ':' {
			checkPass = true
			break
		}
	}

	if checkPass == true {
		srv.AddPeerAsync(peer)
		c.JSON(http.StatusOK, OpSuccessMsg)
	} else {
		c.JSON(http.StatusBadRequest, InvalidPeerErr)
	}
	return
}

func (srv *PioplatServer) setPeriRatio(c *gin.Context) {
	var (
		err1   error
		err2   error
		err3   error
		token  string
		ratio0 float64
		ratio1 float64
		ratio2 float64
	)
	token = c.PostForm(TokenKey)
	if token != srv.adminToken {
		c.JSON(http.StatusBadRequest, AuthFailedErr)
		return
	}

	ratio0, err1 = strconv.ParseFloat(c.PostForm(Ratio0Key), 64)
	ratio1, err2 = strconv.ParseFloat(c.PostForm(Ratio1Key), 64)
	ratio2, err3 = strconv.ParseFloat(c.PostForm(Ratio2Key), 64)
	if err1 != nil || err2 != nil || err3 != nil {
		c.JSON(http.StatusBadRequest, InvalidFloatErr)
		return
	}
	ratio0 /= 100
	ratio1 /= 100
	ratio2 /= 100
	if ratio0+ratio1+ratio2 < 0.95 || ratio0+ratio1+ratio2 > 1.05 {
		c.JSON(http.StatusOK, InvalidFloatErr)
		return
	}

	srv.setRatiosFn(ratio0, ratio1, ratio2)
	c.JSON(http.StatusOK, OpSuccessMsg)
	return
}

func (srv *PioplatServer) sendtxHandler(c *gin.Context) {
	var (
		token     string
		txEncoded string
		txRLP     []byte
		tx        = &types.Transaction{}
		err       error
		checkPass = false
	)
	token = c.PostForm(token)
	txEncoded = c.PostForm(TxEncodedKey)
	if token == srv.adminToken {
		checkPass = true
	}
	if checkPass == false {
		// todo: fetch token from backend server
	}

	if checkPass == true {
		txRLP, err = base64.StdEncoding.DecodeString(txEncoded)
		if err != nil {
			c.JSON(http.StatusBadRequest, InvalidBase64TxErr)
			return
		}
		err = rlp.DecodeBytes(txRLP, tx)
		if err != nil {
			c.JSON(http.StatusBadRequest, InvalidRlpTxErr)
			return
		}

		srv.handler.peers.lock.Lock()
		for _, p := range srv.handler.peers.peers {
			go p.SendTransactions(types.Transactions{tx})
		}
		srv.handler.peers.lock.Unlock()

		c.JSON(http.StatusOK, OpSuccessMsg)
	}
	return
}
