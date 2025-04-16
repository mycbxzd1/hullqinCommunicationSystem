package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// CustomClaims 定义 JWT 自定义 Claims，用于解析 badge 与 color 字段
type CustomClaims struct {
	Badge string `json:"badge"`
	Color string `json:"color"`
	jwt.StandardClaims
}

// ChatMessage 表示聊天消息（普通消息或系统消息）
type ChatMessage struct {
	Type       string    `json:"type"`                 // "message" 或 "systemMessage"
	Sender     string    `json:"sender,omitempty"`     // 发送者，系统消息时可为空或设为 "系统"
	Content    string    `json:"content"`              // 消息内容
	Timestamp  time.Time `json:"timestamp"`            // 发送时间
	Badge      string    `json:"badge,omitempty"`      // 用户徽章名称（普通消息）
	BadgeColor string    `json:"badge_color,omitempty"`// 用户徽章颜色（十六进制）
}

// User 表示在线用户信息
type User struct {
	Username     string
	UUID         string
	Conn         *websocket.Conn
	ActiveLogout bool      // 是否主动注销（非超时注销）
	Badge        string    // 解析后的徽章名称
	BadgeColor   string    // 解析后的徽章颜色，默认 "#FF0000"
	LastActive   time.Time // 最后一次发送消息时间
}

// Room 表示一个聊天室
type Room struct {
	RoomID  string
	Users   map[string]*User   // 在线用户，key 为用户 UUID
	History []ChatMessage      // 历史消息
	Mutex   sync.Mutex         // 保护房间内数据
}

var (
	rooms      = make(map[string]*Room)
	roomsMutex sync.RWMutex

	uuidRoomMap = make(map[string]string)
	uuidMutex   sync.RWMutex

	// 用于写日志的互斥锁
	logMutex sync.Mutex

	// 全局 JWT 密钥（用于解析 extend 参数）
	jwtSecret string
)

// 管理员密码，从环境变量 ADMIN_PASSWORD 读取
var adminPassword string

// WebSocket 升级器
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// joinRequest 请求：加入房间时提交
type joinRequest struct {
	RoomID   string `json:"roomid"`
	Username string `json:"username"`
	Extend   string `json:"extend,omitempty"` // 可选，Base64 再编码的 JWT
}

// joinResponse 响应：返回 WebSocket 地址
type joinResponse struct {
	WSURL string `json:"ws_url"`
}

// logoutRequest 请求：注销用户
type logoutRequest struct {
	RoomID   string `json:"roomid"`
	Username string `json:"username"`
	UUID     string `json:"uuid"`
}

// adminCloseRequest 请求：管理员关闭房间
type adminCloseRequest struct {
	RoomID   string `json:"roomid"`
	Password string `json:"password"`
}

// nopResponse 响应：返回房间人数
type nopResponse struct {
	Number int `json:"nop"`
}

// trimUsername 限制用户名长度：英文算 1，其余算 2，超过限制时返回原字符串的后缀使有效长度不超过 30
func trimUsername(username string) string {
	runes := []rune(username)
	eff := 0
	for _, r := range runes {
		if r < 128 {
			eff++
		} else {
			eff += 2
		}
	}
	if eff <= 30 {
		return username
	}
	// 从右侧截取，使得有效长度不超过30
	eff = 0
	idx := len(runes)
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] < 128 {
			eff++
		} else {
			eff += 2
		}
		if eff > 30 {
			idx = i + 1
			break
		}
	}
	return string(runes[idx:])
}

// getUniqueUsername 检查房间内是否有相同用户名，如有则在后面追加序号（格式 001、002）
func getUniqueUsername(room *Room, baseName string) string {
	room.Mutex.Lock()
	defer room.Mutex.Unlock()
	count := 0
	for _, user := range room.Users {
		if user.Username == baseName || strings.HasPrefix(user.Username, baseName) {
			count++
		}
	}
	if count == 0 {
		return baseName
	}
	return fmt.Sprintf("%s%03d", baseName, count+1)
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

// broadcastSystemMessage 发送系统消息（类型 systemMessage）到房间，并保存到历史记录及写日志
func broadcastSystemMessage(room *Room, content string) {
	msg := ChatMessage{
		Type:      "systemMessage",
		Sender:    "系统",
		Content:   content,
		Timestamp: time.Now(),
	}
	room.Mutex.Lock()
	room.History = append(room.History, msg)
	room.Mutex.Unlock()
	broadcastMessage(room, msg)
	recordChatLog(msg)
}

// getWSHandler 处理 POST /api/getws 接口：用户加入房间
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

	// 限制用户名长度
	trimmedName := trimUsername(req.Username)

	// 生成唯一 UUID
	id := uuid.New().String()

	var badge string
	color := "#FF0000" // 默认大红色

	// 如果提供了 extend 参数，则解析 Base64 编码的 JWT 获取 badge 和 color
	if req.Extend != "" {
		decoded, err := base64.StdEncoding.DecodeString(req.Extend)
		if err != nil {
			log.Printf("Base64 解码 extend 失败: %v", err)
		} else {
			var claims CustomClaims
			token, err := jwt.ParseWithClaims(string(decoded), &claims, func(token *jwt.Token) (interface{}, error) {
				if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, fmt.Errorf("unexpected signing method")
				}
				return []byte(jwtSecret), nil
			})
			if err != nil {
				log.Printf("JWT 解析失败: %v", err)
			} else if token.Valid {
				badge = claims.Badge
				if claims.Color != "" {
					color = claims.Color
				}
			} else {
				log.Printf("JWT 无效")
			}
		}
	}

	room := getOrCreateRoom(req.RoomID)
	// 限制相同用户名
	uniqueName := getUniqueUsername(room, trimmedName)
	room.Mutex.Lock()
	room.Users[id] = &User{
		Username:     uniqueName,
		UUID:         id,
		Conn:         nil,
		ActiveLogout: false,
		Badge:        badge,
		BadgeColor:   color,
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

// wsHandler 处理 WebSocket 连接
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

	// 在用户成功连接后，广播系统消息通知“xxx加入了房间”
	joinMsg := fmt.Sprintf("人员变更: %s加入了房间", user.Username)
	broadcastSystemMessage(room, joinMsg)

	// 启动 Ping 机制（每 30 秒发送一次 ping）
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

	// 消息读取循环
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

		switch msg["type"] {
		case "message":
			chatMsg := ChatMessage{
				Type:       "message",
				Sender:     user.Username,
				Content:    msg["content"],
				Timestamp:  time.Now(),
				Badge:      user.Badge,
				BadgeColor: user.BadgeColor,
			}
			room.Mutex.Lock()
			room.History = append(room.History, chatMsg)
			room.Mutex.Unlock()
			broadcastMessage(room, chatMsg)
			recordChatLog(chatMsg)
		case "systemMessage":
			// 客户端发送的系统消息，直接保存并广播
			sysMsg := ChatMessage{
				Type:      "systemMessage",
				Sender:    "系统",
				Content:   msg["content"],
				Timestamp: time.Now(),
			}
			room.Mutex.Lock()
			room.History = append(room.History, sysMsg)
			room.Mutex.Unlock()
			broadcastMessage(room, sysMsg)
			recordChatLog(sysMsg)
		}
	}
	conn.Close()
	// 当连接断开时，如果非主动注销则不删除（自动断开只记录日志），
	// 否则广播系统消息通知“xxx退出了房间”
	if user.ActiveLogout {
		exitMsg := fmt.Sprintf("人员变更: %s退出了房间", user.Username)
		broadcastSystemMessage(room, exitMsg)
		log.Printf("用户 %s(%s)已主动注销并断开连接", user.Username, user.UUID)
	} else {
		log.Printf("WebSocket 异常断开（自动断开）用户 %s(%s)，保留状态", user.Username, user.UUID)
	}
}

// broadcastMessage 将消息广播到房间中所有在线用户
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

// recordChatLog 将消息写入日志文件，保存到 logs/YYYYMMDD/chat.log
func recordChatLog(chatMsg ChatMessage) {
	dateStr := chatMsg.Timestamp.Format("20060102")
	dailyLogDir := filepath.Join("logs", dateStr)
	if err := os.MkdirAll(dailyLogDir, 0755); err != nil {
		log.Printf("创建日志目录失败: %v", err)
		return
	}
	logFilePath := filepath.Join(dailyLogDir, "chat.log")
	logEntry := fmt.Sprintf("%s [%s] (badge: %s, color: %s): %s\n",
		chatMsg.Timestamp.Format("2006-01-02 15:04:05"),
		chatMsg.Sender, chatMsg.Badge, chatMsg.BadgeColor, chatMsg.Content)
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

// historyHandler GET /api/history?roomid=[roomid] 返回房间内历史消息（包含 badge 和 badge_color）
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

// logoutHandler 处理 POST /api/logout 接口：用户主动注销
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

	// 广播系统消息
	exitMsg := fmt.Sprintf("人员变更: %s退出了房间", user.Username)
	broadcastSystemMessage(room, exitMsg)

	removeUser(req.UUID, req.RoomID)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "注销成功")
}

// adminCloseHandler 处理 POST /api/close 接口：管理员关闭房间
func adminCloseHandler(w http.ResponseWriter, r *http.Request) {
	var req adminCloseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}
	if req.RoomID == "" || req.Password == "" {
		http.Error(w, "参数缺失", http.StatusBadRequest)
		return
	}
	// 检查管理员密码
	if req.Password != adminPassword {
		http.Error(w, "管理员密码错误", http.StatusUnauthorized)
		return
	}
	roomsMutex.RLock()
	room, exists := rooms[req.RoomID]
	roomsMutex.RUnlock()
	if !exists {
		http.Error(w, "房间不存在", http.StatusBadRequest)
		return
	}
	// 广播系统消息通知房间被强制关闭
	adminMsg := "管理员强制关闭本房间,如需清空历史记录请刷新页面"
	broadcastSystemMessage(room, adminMsg)
	// 关闭房间内所有用户的连接
	room.Mutex.Lock()
	for _, user := range room.Users {
		if user.Conn != nil {
			closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, adminMsg)
			user.Conn.WriteMessage(websocket.CloseMessage, closeMsg)
			user.Conn.Close()
		}
	}
	room.Mutex.Unlock()
	// 归档房间历史记录并删除房间
	archiveRoom(room)
	roomsMutex.Lock()
	delete(rooms, req.RoomID)
	roomsMutex.Unlock()

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "房间已关闭")
}

// nopHandler 处理 GET /api/nop?roomid=[roomid] 接口，返回房间在线人数
func nopHandler(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("roomid")
	if roomID == "" {
		http.Error(w, "缺少 roomid 参数", http.StatusBadRequest)
		return
	}
	roomsMutex.RLock()
	room, exists := rooms[roomID]
	roomsMutex.RUnlock()
	nop := 0
	if exists {
		room.Mutex.Lock()
		nop = len(room.Users)
		room.Mutex.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(nopResponse{Number: nop})
}

// removeUser 删除房间中指定用户，并在房间为空时归档历史数据
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

// autoLogoutInactiveUsers 每分钟检查一次，对于超过 1 小时未活跃的用户，自动注销并广播“退出房间”系统消息
func autoLogoutInactiveUsers() {
	ticker := time.NewTicker(1 * time.Minute)
	for range ticker.C {
		now := time.Now()
		var emptyRooms []string
		roomsMutex.RLock()
		for _, room := range rooms {
			room.Mutex.Lock()
			for uuid, user := range room.Users {
				if !user.ActiveLogout && now.Sub(user.LastActive) > time.Hour {
					log.Printf("用户 %s(%s)超时自动注销", user.Username, user.UUID)
					// 广播退出消息
					exitMsg := fmt.Sprintf("人员变更: %s退出了房间", user.Username)
					broadcastSystemMessage(room, exitMsg)
					user.ActiveLogout = true
					// 删除该用户
					delete(room.Users, uuid)
					uuidMutex.Lock()
					delete(uuidRoomMap, uuid)
					uuidMutex.Unlock()
				}
			}
			if len(room.Users) == 0 {
				emptyRooms = append(emptyRooms, room.RoomID)
			}
			room.Mutex.Unlock()
		}
		roomsMutex.RUnlock()

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
	// 初始化 JWT 密钥，若环境变量 JWT_SECRET 为空，则自动生成并输出
	jwtSecret = os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		jwtSecret = uuid.New().String()
		log.Printf("未检测到 JWT_SECRET，自动生成: %s", jwtSecret)
	} else {
		log.Printf("使用 JWT_SECRET: %s", jwtSecret)
	}

	// 读取管理员密码（若未设置，则默认为空，调用管理员接口时将无法通过）
	adminPassword = os.Getenv("ADMIN_PASSWORD")
	if adminPassword == "" {
		log.Printf("未设置管理员密码 ADMIN_PASSWORD")
	} else {
		log.Printf("使用管理员密码 ADMIN_PASSWORD")
	}

	go autoLogoutInactiveUsers()

	router := mux.NewRouter()
	router.HandleFunc("/api/getws", getWSHandler).Methods("POST")
	router.HandleFunc("/api/history", historyHandler).Methods("GET")
	router.HandleFunc("/api/logout", logoutHandler).Methods("POST")
	router.HandleFunc("/api/close", adminCloseHandler).Methods("POST")
	router.HandleFunc("/api/nop", nopHandler).Methods("GET")
	router.HandleFunc("/ws/{uuid}", wsHandler)

	addr := ":8080"
	log.Printf("服务器启动，监听 %s", addr)
	log.Fatal(http.ListenAndServe(addr, router))
}
