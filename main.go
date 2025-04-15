package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// ChatMessage 定义聊天记录
type ChatMessage struct {
	Type      string    `json:"type"`      // 消息类型，目前固定为 "message"
	Sender    string    `json:"sender"`    // 发送者用户名
	Content   string    `json:"content"`   // 消息内容
	Timestamp time.Time `json:"timestamp"` // 发送时间
}

// User 定义在线用户
type User struct {
	Username      string
	UUID          string
	Conn          *websocket.Conn
	ActiveLogout  bool // 标记是否主动注销，默认为 false
}

// Room 定义聊天室
type Room struct {
	RoomID  string
	Users   map[string]*User // key 是用户的 UUID
	History []ChatMessage    // 聊天记录列表
	Mutex   sync.Mutex       // 保护当前房间数据的并发访问
}

// 全局房间管理
var (
	rooms      = make(map[string]*Room)
	roomsMutex sync.RWMutex
	// 辅助映射 uuid 到对应房间ID，便于 WS 连接查找
	uuidRoomMap = make(map[string]string)
	uuidMutex   sync.RWMutex
)

// 用于日志写入的互斥锁
var logMutex sync.Mutex

// WebSocket 升级器
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// 请求结构体
type joinRequest struct {
	RoomID   string `json:"roomid"`
	Username string `json:"username"`
}

// 返回的 WS 地址结构
type joinResponse struct {
	WSURL string `json:"ws_url"`
}

// logoutRequest 定义注销用户传入参数
type logoutRequest struct {
	RoomID   string `json:"roomid"`
	Username string `json:"username"`
	UUID     string `json:"uuid"`
}

// getOrCreateRoom 获取或创建一个房间
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

// POST /api/getws 处理加入房间请求
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

	// 生成唯一 UUID
	id := uuid.New().String()

	// 获取或新建房间，并注册用户（初始时 WebSocket 连接尚未建立，Conn 为 nil）
	room := getOrCreateRoom(req.RoomID)
	room.Mutex.Lock()
	room.Users[id] = &User{
		Username:     req.Username,
		UUID:         id,
		Conn:         nil,
		ActiveLogout: false,
	}
	room.Mutex.Unlock()

	// 在全局映射中记录 uuid 对应的房间
	uuidMutex.Lock()
	uuidRoomMap[id] = req.RoomID
	uuidMutex.Unlock()

	// 返回 WebSocket 连接地址，实际部署时可返回完整地址
	resp := joinResponse{
		WSURL: "/ws/" + id,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// wsHandler 处理 WebSocket 连接 /ws/{uuid}
func wsHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	uuidParam := vars["uuid"]
	if uuidParam == "" {
		http.Error(w, "缺少 uuid", http.StatusBadRequest)
		return
	}

	// 根据 uuid 查找对应房间
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

	// 获取用户信息
	room.Mutex.Lock()
	user, exists := room.Users[uuidParam]
	room.Mutex.Unlock()
	if !exists {
		http.Error(w, "用户不存在", http.StatusBadRequest)
		return
	}

	// 升级为 WebSocket 连接
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket 升级失败: %v", err)
		return
	}
	user.Conn = conn
	log.Printf("用户 %s(%s) 加入房间 %s", user.Username, user.UUID, room.RoomID)

	// 读取客户端消息
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			// 如果连接异常关闭（例如 1006），不视为主动注销，保留用户信息
			if closeErr, ok := err.(*websocket.CloseError); ok {
				log.Printf("WebSocket 关闭码：%d, 原因: %s", closeErr.Code, closeErr.Text)
			} else {
				log.Printf("读取 WebSocket 消息失败: %v", err)
			}
			break // 直接退出循环，不调用 removeUser
		}
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
			}
			// 保存聊天记录到房间内存中
			room.Mutex.Lock()
			room.History = append(room.History, chatMsg)
			room.Mutex.Unlock()

			// 广播消息到其他在线用户
			broadcastMessage(room, chatMsg)

			// 将消息记录到日志文件（按天分目录）
			recordChatLog(chatMsg)
		}
	}
	// 在此处关闭连接后，不调用 removeUser，只有主动调用 logout 时才删除用户记录
	conn.Close()
	if !user.ActiveLogout {
		log.Printf("WebSocket 异常断开(自动断开) 用户 %s(%s)，保留用户状态", user.Username, user.UUID)
	} else {
		log.Printf("用户 %s(%s)已主动注销，连接已断开", user.Username, user.UUID)
	}
}

// 广播消息到房间内所有在线用户
func broadcastMessage(room *Room, msg ChatMessage) {
	room.Mutex.Lock()
	defer room.Mutex.Unlock()
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("序列化消息失败: %v", err)
		return
	}
	for _, u := range room.Users {
		// 如果用户连接未建立或已断开则跳过
		if u.Conn != nil {
			if err := u.Conn.WriteMessage(websocket.TextMessage, data); err != nil {
				log.Printf("向用户 %s 发送消息失败: %v", u.Username, err)
			}
		}
	}
}

// recordChatLog 将聊天消息写入日志文件，日志按照日期生成目录保存
func recordChatLog(chatMsg ChatMessage) {
	// 按消息日期生成目录（格式：YYYYMMDD）
	dateStr := chatMsg.Timestamp.Format("20060102")
	dailyLogDir := filepath.Join("logs", dateStr)
	if err := os.MkdirAll(dailyLogDir, 0755); err != nil {
		log.Printf("创建日志目录失败: %v", err)
		return
	}
	// 拼接日志文件路径，文件名为 chat.log
	logFilePath := filepath.Join(dailyLogDir, "chat.log")
	// 格式化日志内容，例如： "2023-10-15 14:23:45 [Alice]: Hello World"
	logEntry := fmt.Sprintf("%s [%s]: %s\n", chatMsg.Timestamp.Format("2006-01-02 15:04:05"), chatMsg.Sender, chatMsg.Content)
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

// GET /api/history?roomid=[roomid] 处理获取历史消息
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

// POST /api/logout 处理用户主动注销请求
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
	// 标记为主动注销
	user.ActiveLogout = true
	// 使 uuid 立即失效（从全局映射中移除）
	uuidMutex.Lock()
	delete(uuidRoomMap, req.UUID)
	uuidMutex.Unlock()
	room.Mutex.Unlock()

	// 如果 WebSocket 连接还在，则主动发送关闭帧断开连接
	if user.Conn != nil {
		closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "主动注销")
		user.Conn.WriteMessage(websocket.CloseMessage, closeMsg)
		user.Conn.Close()
	}

	// 主动注销则调用 removeUser 删除用户记录
	removeUser(req.UUID, req.RoomID)

	// 返回注销成功
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "注销成功")
}

// removeUser 从指定房间中删除用户，并检查房间中是否所有用户都是主动注销后清空房间并归档
func removeUser(uuidStr, roomID string) {
	roomsMutex.RLock()
	room, exists := rooms[roomID]
	roomsMutex.RUnlock()
	if !exists {
		return
	}
	
	// 删除用户记录（主动注销的用户会被删除）
	room.Mutex.Lock()
	delete(room.Users, uuidStr)
	userCount := len(room.Users)
	room.Mutex.Unlock()

	// 如果房间中所有用户均已注销（即房间为空），归档房间历史记录并清除房间
	if userCount == 0 {
		log.Printf("房间 %s 所有用户已主动注销，归档历史记录并清空数据", roomID)
		archiveRoom(room)
		roomsMutex.Lock()
		delete(rooms, roomID)
		roomsMutex.Unlock()
	}
}

// archiveRoom 将房间历史记录归档到 data 目录，文件名包含归档时间
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
	router := mux.NewRouter()
	router.HandleFunc("/api/getws", getWSHandler).Methods("POST")
	router.HandleFunc("/api/history", historyHandler).Methods("GET")
	router.HandleFunc("/api/logout", logoutHandler).Methods("POST")
	router.HandleFunc("/ws/{uuid}", wsHandler)
	addr := ":8080"
	log.Printf("服务器启动，监听 %s", addr)
	log.Fatal(http.ListenAndServe(addr, router))
}
