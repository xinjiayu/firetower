package manager

import (
	"container/list"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	pb "github.com/holdno/firetower/grpc/manager"
	"github.com/holdno/firetower/socket"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
)

type Manager struct {
}

var (
	topicRelevance sync.Map
	ConnIndexTable sync.Map

	Logger             func(types, info string) = log // 接管系统log t log类型 info log信息
	LogLevel                                    = "INFO"
	DefaultWriter      io.Writer                = os.Stdout
	DefaultErrorWriter io.Writer                = os.Stderr
)

type topicRelevanceItem struct {
	ip   string
	num  int64
	conn net.Conn
}

type topicGrpcService struct {
	mu sync.RWMutex
}

// Publish
func (t *topicGrpcService) Publish(ctx context.Context, request *pb.PublishRequest) (*pb.PublishResponse, error) {
	Logger("INFO", fmt.Sprintf("new message: %s", string(request.Data)))

	value, ok := topicRelevance.Load(request.Topic)
	if !ok {
		// topic 没有存在订阅列表中直接过滤
		return &pb.PublishResponse{Ok: false}, errors.New("topic not exist")
	} else {

		table := value.(*list.List)
		t.mu.Lock()
		for e := table.Front(); e != nil; e = e.Next() {
			c, ok := ConnIndexTable.Load(e.Value.(*topicRelevanceItem).ip)
			if ok {
				b, err := socket.Enpack(socket.PublishKey, request.MessageId, request.Source, request.Topic, request.Data)
				if err != nil {

				}
				_, err = c.(*connectBucket).conn.Write(b)
				if err != nil {
					c.(*connectBucket).close()
				}
			}
		}
		t.mu.Unlock()
	}

	return &pb.PublishResponse{Ok: true}, nil
}

// 获取topic订阅数
func (t *topicGrpcService) GetConnectNum(ctx context.Context, request *pb.GetConnectNumRequest) (*pb.GetConnectNumResponse, error) {
	value, ok := topicRelevance.Load(request.Topic)
	var num int64
	if ok {
		l, _ := value.(*list.List)
		for e := l.Front(); e != nil; e = e.Next() {
			num += e.Value.(*topicRelevanceItem).num
		}
	}
	return &pb.GetConnectNumResponse{Number: num}, nil
}

// topic 订阅
func (t *topicGrpcService) SubscribeTopic(ctx context.Context, request *pb.SubscribeTopicRequest) (*pb.SubscribeTopicResponse, error) {
	for _, topic := range request.Topic {
		var store *list.List
		value, ok := topicRelevance.Load(topic)

		if !ok {
			// topic map 里面维护一个链表
			store = list.New()
			store.PushBack(&topicRelevanceItem{
				ip:  request.Ip,
				num: 1,
			})
			topicRelevance.Store(topic, store)
		} else {
			store = value.(*list.List)
			for e := store.Front(); e != nil; e = e.Next() {
				if e.Value.(*topicRelevanceItem).ip == request.Ip {
					e.Value.(*topicRelevanceItem).num++
				}
			}
		}
	}
	return &pb.SubscribeTopicResponse{}, nil
}

// topic 取消订阅
func (t *topicGrpcService) UnSubscribeTopic(ctx context.Context, request *pb.UnSubscribeTopicRequest) (*pb.UnSubscribeTopicResponse, error) {
	for _, topic := range request.Topic {
		value, ok := topicRelevance.Load(topic)

		if !ok {
			// topic 没有存在订阅列表中直接过滤
			continue
		} else {
			store := value.(*list.List)
			for e := store.Front(); e != nil; e = e.Next() {
				if e.Value.(*topicRelevanceItem).ip == request.Ip {
					if e.Value.(*topicRelevanceItem).num-1 == 0 {
						store.Remove(e)
						if store.Len() == 0 {
							topicRelevance.Delete(topic)
						}
					} else {
						// 这里修改是直接修改map内部值
						e.Value.(*topicRelevanceItem).num--
					}
					break
				}
			}
		}
	}
	return &pb.UnSubscribeTopicResponse{}, nil
}

func (m *Manager) StartGrpcService(port string) {
	lis, err := net.Listen("tcp", port)
	if err != nil {
		Logger("ERROR", fmt.Sprintf("grpc service listen error: %v", err))
		panic(fmt.Sprintf("grpc service listen error: %v", err))
	}
	s := grpc.NewServer()
	pb.RegisterTopicServiceServer(s, &topicGrpcService{})
	s.Serve(lis)
}

type connectBucket struct {
	overflow   []byte
	packetChan chan *socket.SendMessage
	conn       net.Conn
	isClose    bool
	closeChan  chan struct{}
	mu         sync.Mutex
}

func (m *Manager) StartSocketService(addr string) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		Logger("ERROR", fmt.Sprintf("tcp service listen error: %v", err))
		return
	}
	Logger("INFO", fmt.Sprintf("tcp service listening: %s", addr))
	for {
		conn, err := lis.Accept()
		if err != nil {
			Logger("ERROR", fmt.Sprintf("tcp service accept error: %v", err))
			continue
		}
		bucket := &connectBucket{
			overflow:   make([]byte, 0),
			packetChan: make(chan *socket.SendMessage, 1024),
			conn:       conn,
			isClose:    false,
			closeChan:  make(chan struct{}),
		}
		bucket.relation()     // 建立连接关系
		go bucket.sendLoop()  // 发包
		go bucket.handler()   // 接收字节流并解包
		go bucket.heartbeat() // 心跳
	}
}

func log(types, info string) {
	if types == "INFO" {
		if LogLevel != "INFO" {
			return
		}
		fmt.Fprintf(
			DefaultWriter,
			"[Firetower Manager] %s %s %s | LOGTIME %s | LOG %s\n",
			socket.Green, types, socket.Reset,
			time.Now().Format("2006-01-02 15:04:05"),
			info)
	} else {
		fmt.Fprintf(
			DefaultErrorWriter,
			"[Firetower Manager] %s %s %s | LOGTIME %s | LOG %s\n",
			socket.Red, types, socket.Reset,
			time.Now().Format("2006-01-02 15:04:05"),
			info)
	}
}

func (c *connectBucket) relation() {
	// 维护一个IP->连接关系的索引map
	_, ok := ConnIndexTable.Load(c.conn.RemoteAddr().String())
	if !ok {
		Logger("INFO", fmt.Sprintf("new connection: %s", c.conn.RemoteAddr().String()))
		ConnIndexTable.Store(c.conn.RemoteAddr().String(), c)
	}
}

func (c *connectBucket) delRelation() {
	topicRelevance.Range(func(key, value interface{}) bool {
		store, _ := value.(*list.List)
		for e := store.Front(); e != nil; e = e.Next() {
			if e.Value.(*topicRelevanceItem).ip == c.conn.RemoteAddr().String() {
				store.Remove(e)
			}
		}
		if store.Len() == 0 {
			topicRelevance.Delete(key)
		}
		return true
	})
}

func (c *connectBucket) close() {
	c.mu.Lock()
	if !c.isClose {
		c.isClose = true
		close(c.closeChan)
		c.conn.Close()
		c.delRelation() // 删除topic绑定关系
	}
	c.mu.Unlock()
}

func (c *connectBucket) handler() {
	for {
		var buffer = make([]byte, 1024*16)
		l, err := c.conn.Read(buffer)
		if err != nil {
			c.close()
			return
		}
		c.overflow, err = socket.Depack(append(c.overflow, buffer[:l]...), c.packetChan)
		if err != nil {
			Logger("ERROR", err.Error())
		}
	}
}

func (c *connectBucket) sendLoop() {
	for {
		select {
		case message := <-c.packetChan:

			value, ok := topicRelevance.Load(message.Topic)
			if !ok {
				// topic 没有存在订阅列表中直接过滤
				continue
			} else {
				table := value.(*list.List)
				for e := table.Front(); e != nil; e = e.Next() {
					bucket, ok := ConnIndexTable.Load(e.Value.(*topicRelevanceItem).ip)
					if ok {
						bytes, err := socket.Enpack(message.Type, message.Context.Id, message.Context.Source, message.Topic, message.Data)
						if err != nil {
							Logger("ERROR", fmt.Sprintf("protocol 封包时错误，%v", err))
						}
						_, err = bucket.(*connectBucket).conn.Write(bytes)
						if err != nil {
							// 直接操作table.Remove 可以改变map中list的值
							bucket.(*connectBucket).close()
							return
						}
					}
				}
			}
			message.Info("topic manager sended")
			message.Recycling()
		case <-c.closeChan:
			return
		}
	}
}

func (c *connectBucket) heartbeat() {
	t := time.NewTicker(1 * time.Minute)
	for {
		<-t.C
		b, _ := socket.Enpack("heartbeat", "0", "system", "*", []byte("heartbeat"))
		_, err := c.conn.Write(b)
		if err != nil {
			c.close()
			return
		}
	}
}
