package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/golang-jwt/jwt" // 使用 golang-jwt/jwt 库
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// ChatMessage 表示聊天消息，包含用户徽章字段（bandge）
type ChatMessage struct {
	Type      string    `json:"type"`                // 固定为 "message"
	Sender    string    `json:"sender"`              // 发送者用户名
	Content   string    `json:"content"`             // 消息内容
	Timestamp time.Time `json:"timestamp"`           // 发送时间
	Bandge    string    `json:"bandge,omitempty"`    // 用户徽章，若有
}

// User 表示在线用户信息，增加 LastActive 记录上次发消息时间
type User struct {
	Username     string
	UUID         string
	Conn         *websocket.Conn
	ActiveLogout bool      // 主动注销标记
	Badge        string    // JWT 中解析的徽章（字段名 bandge）
	LastActive   time.Time // 最后一次发送消息时间
}

// Room 表示聊天室
type Room struct {
	RoomID  string
	Users   map[string]*User // key：UUID
	History []ChatMessage    // 聊天记录集合
	Mutex   sync.Mutex       // 保护并发
}

var (
	rooms      = make(map[string]*Room)
	roomsMutex sync.RWMutex

	uuidRoomMap = make(map[string]string)
	uuidMutex   sync.RWMutex

	// 用于日志写入的互斥锁
	logMutex sync.Mutex

	// 全局 JWT 密钥
	jwtSecret string
)

// WebSocket 升级器
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// joinRequest 用户加入房间请求参数，增加 extend 字段
type joinRequest struct {
	RoomID   string `json:"roomid"`
	Username string `json:"username"`
	Extend   string `json:"extend,omitempty"` // 可选：Base64 再编码的 JWT
}

// joinResponse 返回 WebSocket 地址
type joinResponse struct {
	WSURL string `json:"ws_url"`
}

// logoutRequest 用户注销请求参数
type logoutRequest struct {
	RoomID   string `json:"roomid"`
	Username string `json:"username"`
	UUID     string `json:"uuid"`
}

// getOrCreateRoom 获取或创建房间
func getOrCreateRoom(roomID string) *Room {
	roomsMutex.Lock()
	defer roomsMutex.Unlock()
	room, exists := rooms[roomID]
	if !exists {
		room = &Room{
			RoomID:  roomID,
			Users:   make(map[string]*User),
			History: []ChatMessage{},
		}
		rooms[roomID] = room
	}
	return room
}

// POST /api/getws 接口：加入房间获取 WebSocket 连接地址，同时解析 extend 参数中的徽章信息
func getWSHandler(w http.ResponseWriter, r *http.Request) {
	var req joinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}
	if req.RoomID == "" || req.Username == "" {
		http.Error(w, "房间号和用户名不能为空", http.StatusBadRequest)
		return
	}

	// 生成用户唯一 UUID
	id := uuid.New().String()

	var badge string
	// 如果传入 extend 参数，则尝试解析 JWT，获取 bandge 字段
	if req.Extend != "" {
		decoded, err := base64.StdEncoding.DecodeString(req.Extend)
		if err != nil {
			log.Printf("Base64 解码 extend 失败: %v", err)
		} else {
			token, err := jwt.Parse(string(decoded), func(token *jwt.Token) (interface{}, error) {
				if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, fmt.Errorf("unexpected signing method")
				}
				return []byte(jwtSecret), nil
			})
			if err != nil {
				log.Printf("JWT 解析失败: %v", err)
			} else if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
				if b, exists := claims["bandge"]; exists {
					if bs, ok := b.(string); ok {
						badge = bs
					}
				}
			} else {
				log.Printf("JWT 无效或错误的 Claims")
			}
		}
	}

	room := getOrCreateRoom(req.RoomID)
	room.Mutex.Lock()
	room.Users[id] = &User{
		Username:     req.Username,
		UUID:         id,
		Conn:         nil,
		ActiveLogout: false,
		Badge:        badge,
		LastActive:   time.Now(),
	}
	room.Mutex.Unlock()

	uuidMutex.Lock()
	uuidRoomMap[id] = req.RoomID
	uuidMutex.Unlock()

	resp := joinResponse{WSURL: "/ws/" + id}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// wsHandler 处理 WebSocket 连接，添加心跳 Ping 以及更新 LastActive 时间
func wsHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	uuidParam := vars["uuid"]
	if uuidParam == "" {
		http.Error(w, "缺少 uuid", http.StatusBadRequest)
		return
	}

	uuidMutex.RLock()
	roomID, exists := uuidRoomMap[uuidParam]
	uuidMutex.RUnlock()
	if !exists {
		http.Error(w, "无效的 uuid", http.StatusBadRequest)
		return
	}

	roomsMutex.RLock()
	room, exists := rooms[roomID]
	roomsMutex.RUnlock()
	if !exists {
		http.Error(w, "房间不存在", http.StatusBadRequest)
		return
	}

	room.Mutex.Lock()
	user, exists := room.Users[uuidParam]
	room.Mutex.Unlock()
	if !exists {
		http.Error(w, "用户不存在", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket 升级失败: %v", err)
		return
	}
	user.Conn = conn
	log.Printf("用户 %s(%s) 加入房间 %s", user.Username, user.UUID, room.RoomID)

	// 启动 Ping 机制，每 30 秒发送一次 ping
	go func(c *websocket.Conn) {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			<-ticker.C
			if err := c.WriteMessage(websocket.PingMessage, []byte("ping")); err != nil {
				log.Printf("Ping 发送失败: %v", err)
				return
			}
		}
	}(conn)

	// 读取消息循环
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if closeErr, ok := err.(*websocket.CloseError); ok {
				log.Printf("WebSocket 关闭码: %d, 原因: %s", closeErr.Code, closeErr.Text)
			} else {
				log.Printf("读取 WebSocket 消息失败: %v", err)
			}
			break
		}

		// 更新用户最后活跃时间
		room.Mutex.Lock()
		user.LastActive = time.Now()
		room.Mutex.Unlock()

		var msg map[string]string
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("解析消息失败: %v", err)
			continue
		}
		if msg["type"] == "message" {
			chatMsg := ChatMessage{
				Type:      "message",
				Sender:    user.Username,
				Content:   msg["content"],
				Timestamp: time.Now(),
				Bandge:    user.Badge,
			}
			// 保存消息到历史记录
			room.Mutex.Lock()
			room.History = append(room.History, chatMsg)
			room.Mutex.Unlock()

			// 广播消息（包括发送者自身）
			broadcastMessage(room, chatMsg)

			// 记录日志到文件
			recordChatLog(chatMsg)
		}
	}
	conn.Close()
	if !user.ActiveLogout {
		log.Printf("WebSocket 异常断开（自动断开）用户 %s(%s)，保留状态", user.Username, user.UUID)
	} else {
		log.Printf("用户 %s(%s)已主动注销，连接已断开", user.Username, user.UUID)
	}
}

// broadcastMessage 将消息广播到房间内所有在线用户
func broadcastMessage(room *Room, msg ChatMessage) {
	room.Mutex.Lock()
	defer room.Mutex.Unlock()
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("序列化消息失败: %v", err)
		return
	}
	for _, u := range room.Users {
		if u.Conn != nil {
			if err := u.Conn.WriteMessage(websocket.TextMessage, data); err != nil {
				log.Printf("向用户 %s 发送消息失败: %v", u.Username, err)
			}
		}
	}
}

// recordChatLog 将消息记录写入日志文件，日志文件存放在 logs/YYYYMMDD/chat.log
func recordChatLog(chatMsg ChatMessage) {
	dateStr := chatMsg.Timestamp.Format("20060102")
	dailyLogDir := filepath.Join("logs", dateStr)
	if err := os.MkdirAll(dailyLogDir, 0755); err != nil {
		log.Printf("创建日志目录失败: %v", err)
		return
	}
	logFilePath := filepath.Join(dailyLogDir, "chat.log")
	logEntry := fmt.Sprintf("%s [%s] (bandge: %s): %s\n", 
		chatMsg.Timestamp.Format("2006-01-02 15:04:05"), chatMsg.Sender, chatMsg.Bandge, chatMsg.Content)
	logMutex.Lock()
	defer logMutex.Unlock()
	f, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("打开日志文件失败: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(logEntry); err != nil {
		log.Printf("写入日志失败: %v", err)
	}
}

// historyHandler GET 历史消息接口，返回包含 bandge 字段的消息列表
func historyHandler(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("roomid")
	if roomID == "" {
		http.Error(w, "缺少 roomid 参数", http.StatusBadRequest)
		return
	}
	roomsMutex.RLock()
	room, exists := rooms[roomID]
	roomsMutex.RUnlock()
	if !exists {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]ChatMessage{})
		return
	}
	room.Mutex.Lock()
	defer room.Mutex.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(room.History)
}

// logoutHandler 主动注销接口，走正常注销流程
func logoutHandler(w http.ResponseWriter, r *http.Request) {
	var req logoutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求数据无效", http.StatusBadRequest)
		return
	}
	if req.RoomID == "" || req.Username == "" || req.UUID == "" {
		http.Error(w, "参数缺失", http.StatusBadRequest)
		return
	}
	roomsMutex.RLock()
	room, exists := rooms[req.RoomID]
	roomsMutex.RUnlock()
	if !exists {
		http.Error(w, "房间不存在", http.StatusBadRequest)
		return
	}
	room.Mutex.Lock()
	user, exists := room.Users[req.UUID]
	if !exists {
		room.Mutex.Unlock()
		http.Error(w, "用户不存在", http.StatusBadRequest)
		return
	}
	user.ActiveLogout = true
	uuidMutex.Lock()
	delete(uuidRoomMap, req.UUID)
	uuidMutex.Unlock()
	room.Mutex.Unlock()

	if user.Conn != nil {
		closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "主动注销")
		user.Conn.WriteMessage(websocket.CloseMessage, closeMsg)
		user.Conn.Close()
	}

	removeUser(req.UUID, req.RoomID)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "注销成功")
}

// removeUser 删除用户记录，并在房间为空时归档历史数据
func removeUser(uuidStr, roomID string) {
	roomsMutex.RLock()
	room, exists := rooms[roomID]
	roomsMutex.RUnlock()
	if !exists {
		return
	}
	room.Mutex.Lock()
	delete(room.Users, uuidStr)
	userCount := len(room.Users)
	room.Mutex.Unlock()

	if userCount == 0 {
		log.Printf("房间 %s 所有用户已注销，归档历史记录并清空数据", roomID)
		archiveRoom(room)
		roomsMutex.Lock()
		delete(rooms, roomID)
		roomsMutex.Unlock()
	}
}

// autoLogoutInactiveUsers 每分钟检查一次，自动注销超过 1 小时未活跃的用户
func autoLogoutInactiveUsers() {
	ticker := time.NewTicker(1 * time.Minute)
	for {
		<-ticker.C
		now := time.Now()
		var emptyRooms []string
		roomsMutex.RLock()
		for _, room := range rooms {
			room.Mutex.Lock()
			for uuid, user := range room.Users {
				if !user.ActiveLogout && now.Sub(user.LastActive) > time.Hour {
					log.Printf("用户 %s(%s)超时自动注销", user.Username, user.UUID)
					user.ActiveLogout = true
					uuidMutex.Lock()
					delete(uuidRoomMap, uuid)
					uuidMutex.Unlock()
					if user.Conn != nil {
						closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "超时自动注销")
						user.Conn.WriteMessage(websocket.CloseMessage, closeMsg)
						user.Conn.Close()
					}
					delete(room.Users, uuid)
				}
			}
			if len(room.Users) == 0 {
				emptyRooms = append(emptyRooms, room.RoomID)
			}
			room.Mutex.Unlock()
		}
		roomsMutex.RUnlock()
		// 删除空房间
		if len(emptyRooms) > 0 {
			roomsMutex.Lock()
			for _, roomID := range emptyRooms {
				delete(rooms, roomID)
			}
			roomsMutex.Unlock()
		}
	}
}

// archiveRoom 将房间历史记录以 JSON 格式归档到 data 目录中
func archiveRoom(room *Room) {
	dataDir := "data"
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Printf("创建 data 目录失败: %v", err)
		return
	}
	filename := fmt.Sprintf("%s_%s.json", room.RoomID, time.Now().Format("20060102_150405"))
	filePath := filepath.Join(dataDir, filename)
	room.Mutex.Lock()
	data, err := json.MarshalIndent(room.History, "", "  ")
	room.Mutex.Unlock()
	if err != nil {
		log.Printf("序列化历史数据失败: %v", err)
		return
	}
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		log.Printf("写入归档文件失败: %v", err)
		return
	}
	log.Printf("归档文件 %s 写入成功", filePath)
}

func main() {
	// 初始化 JWT 密钥：先从环境变量中读取，如果为空则自动生成并输出日志
	jwtSecret = os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		jwtSecret = uuid.New().String()
		log.Printf("未检测到 JWT_SECRET，自动生成: %s", jwtSecret)
	} else {
		log.Printf("使用 JWT_SECRET: %s", jwtSecret)
	}

	// 启动自动注销检查
	go autoLogoutInactiveUsers()

	router := mux.NewRouter()
	router.HandleFunc("/api/getws", getWSHandler).Methods("POST")
	router.HandleFunc("/api/history", historyHandler).Methods("GET")
	router.HandleFunc("/api/logout", logoutHandler).Methods("POST")
	router.HandleFunc("/ws/{uuid}", wsHandler)

	addr := ":8080"
	log.Printf("服务器启动，监听 %s", addr)
	log.Fatal(http.ListenAndServe(addr, router))
}
