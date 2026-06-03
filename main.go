package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Configuration
var (
	lazyFreeLazyExpire = true // configurable via lazyfree-lazy-expire
	activeExpireEffort = 1    // default effort 1 (25% of CPU time per iteration)
	db                 = make(map[string]*Value)
	mu                 sync.RWMutex
)

type Value struct {
	Type      string // "string", "hash"
	Val       interface{}
	ExpiresAt time.Time
}

func main() {
	// Start active expiration cycle
	go activeExpireCycle()

	port := "6379"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}
	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		fmt.Printf("Failed to bind to port %s: %v\n", port, err)
		os.Exit(1)
	}
	defer listener.Close()
	fmt.Printf("Redis server listening on port %s\n", port)

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleConnection(conn)
	}
}

func handleConnection(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		cmd, err := readCommand(reader)
		if err != nil {
			if err != io.EOF {
				fmt.Printf("Error reading command: %v\n", err)
			}
			return
		}
		response := executeCommand(cmd)
		_, err = conn.Write([]byte(response))
		if err != nil {
			return
		}
	}
}

func readCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	if len(line) == 0 || line[0] != '*' {
		return nil, fmt.Errorf("invalid RESP array")
	}
	numArgs, err := strconv.Atoi(line[1:])
	if err != nil {
		return nil, err
	}
	args := make([]string, numArgs)
	for i := 0; i < numArgs; i++ {
		line, err = reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if len(line) == 0 || line[0] != '$' {
			return nil, fmt.Errorf("invalid RESP bulk string length")
		}
		argLen, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, argLen+2) // +2 for \r\n
		_, err = io.ReadFull(reader, buf)
		if err != nil {
			return nil, err
		}
		args[i] = string(buf[:argLen])
	}
	return args, nil
}

func executeCommand(args []string) string {
	if len(args) == 0 {
		return "-ERR empty command\r\n"
	}
	cmd := strings.ToUpper(args[0])
	switch cmd {
	case "PING":
		return "+PONG\r\n"
	case "SET":
		if len(args) < 3 {
			return "-ERR wrong number of arguments for 'set' command\r\n"
		}
		key := args[1]
		val := args[2]
		var expiresAt time.Time
		for i := 3; i < len(args); i++ {
			arg := strings.ToUpper(args[i])
			if arg == "EX" && i+1 < len(args) {
				sec, err := strconv.Atoi(args[i+1])
				if err == nil {
					expiresAt = time.Now().Add(time.Duration(sec) * time.Second)
				}
				i++
			} else if arg == "PX" && i+1 < len(args) {
				msec, err := strconv.Atoi(args[i+1])
				if err == nil {
					expiresAt = time.Now().Add(time.Duration(msec) * time.Millisecond)
				}
				i++
			}
		}
		mu.Lock()
		db[key] = &Value{
			Type:      "string",
			Val:       val,
			ExpiresAt: expiresAt,
		}
		mu.Unlock()
		return "+OK\r\n"
	case "GET":
		if len(args) != 2 {
			return "-ERR wrong number of arguments for 'get' command\r\n"
		}
		key := args[1]
		mu.RLock()
		val, exists := db[key]
		mu.RUnlock()
		if !exists || (!val.ExpiresAt.IsZero() && time.Now().After(val.ExpiresAt)) {
			if exists {
				mu.Lock()
				delete(db, key)
				mu.Unlock()
			}
			return "$-1\r\n"
		}
		if val.Type != "string" {
			return "-WRONGTYPE Operation against a key holding the wrong kind of value\r\n"
		}
		s := val.Val.(string)
		return fmt.Sprintf("$%d\r\n%s\r\n", len(s), s)
	case "HSET":
		if len(args) < 4 || len(args)%2 != 0 {
			return "-ERR wrong number of arguments for 'hset' command\r\n"
		}
		key := args[1]
		mu.Lock()
		val, exists := db[key]
		var h map[string]string
		if !exists || (!val.ExpiresAt.IsZero() && time.Now().After(val.ExpiresAt)) {
			h = make(map[string]string)
			val = &Value{
				Type: "hash",
				Val:  h,
			}
			db[key] = val
		} else {
			if val.Type != "hash" {
				mu.Unlock()
				return "-WRONGTYPE Operation against a key holding the wrong kind of value\r\n"
			}
			h = val.Val.(map[string]string)
		}
		count := 0
		for i := 2; i < len(args); i += 2 {
			field := args[i]
			fieldVal := args[i+1]
			if _, exists := h[field]; !exists {
				count++
			}
			h[field] = fieldVal
		}
		mu.Unlock()
		return fmt.Sprintf(":%d\r\n", count)
	case "EXPIRE":
		if len(args) != 3 {
			return "-ERR wrong number of arguments for 'expire' command\r\n"
		}
		key := args[1]
		sec, err := strconv.Atoi(args[2])
		if err != nil {
			return "-ERR value is not an integer or out of range\r\n"
		}
		mu.Lock()
		val, exists := db[key]
		if !exists || (!val.ExpiresAt.IsZero() && time.Now().After(val.ExpiresAt)) {
			mu.Unlock()
			return ":0\r\n"
		}
		val.ExpiresAt = time.Now().Add(time.Duration(sec) * time.Second)
		mu.Unlock()
		return ":1\r\n"
	case "DEL":
		if len(args) < 2 {
			return "-ERR wrong number of arguments for 'del' command\r\n"
		}
		count := 0
		mu.Lock()
		for i := 1; i < len(args); i++ {
			key := args[i]
			val, exists := db[key]
			if exists && (val.ExpiresAt.IsZero() || time.Now().Before(val.ExpiresAt)) {
				isLarge := false
				if val.Type == "hash" {
					if h, ok := val.Val.(map[string]string); ok && len(h) > 64 {
						isLarge = true
					}
				}
				if isLarge && lazyFreeLazyExpire {
					dbAsyncDelete(key, val)
				} else {
					dbSyncDelete(key)
				}
				count++
			}
		}
		mu.Unlock()
		return fmt.Sprintf(":%d\r\n", count)
	case "CONFIG":
		if len(args) < 2 {
			return "-ERR wrong number of arguments for 'config' command\r\n"
		}
		subCmd := strings.ToUpper(args[1])
		if subCmd == "GET" {
			if len(args) != 3 {
				return "-ERR wrong number of arguments for 'config get' command\r\n"
			}
			param := strings.ToLower(args[2])
			if param == "active-expire-effort" {
				return fmt.Sprintf("*2\r\n$20\r\nactive-expire-effort\r\n$%d\r\n%d\r\n", len(strconv.Itoa(activeExpireEffort)), activeExpireEffort)
			} else if param == "lazyfree-lazy-expire" {
				valStr := "no"
				if lazyFreeLazyExpire {
					valStr = "yes"
				}
				return fmt.Sprintf("*2\r\n$20\r\nlazyfree-lazy-expire\r\n$%d\r\n%s\r\n", len(valStr), valStr)
			} else {
				return "*0\r\n"
			}
		} else if subCmd == "SET" {
			if len(args) != 4 {
				return "-ERR wrong number of arguments for 'config set' command\r\n"
			}
			param := strings.ToLower(args[2])
			val := args[3]
			if param == "active-expire-effort" {
				effort, err := strconv.Atoi(val)
				if err == nil {
					activeExpireEffort = effort
				}
				return "+OK\r\n"
			} else if param == "lazyfree-lazy-expire" {
				if strings.ToLower(val) == "yes" {
					lazyFreeLazyExpire = true
				} else {
					lazyFreeLazyExpire = false
				}
				return "+OK\r\n"
			} else {
				return "-ERR Unknown option or number of arguments for CONFIG SET\r\n"
			}
		}
		return "-ERR unknown CONFIG subcommand\r\n"
	case "INFO":
		section := ""
		if len(args) > 1 {
			section = strings.ToLower(args[1])
		}
		if section == "" || section == "memory" {
			var memStats runtime.MemStats
			runtime.ReadMemStats(&memStats)
			usedMemory := memStats.Alloc
			usedMemoryRSS := getRSS(usedMemory)
			fragRatio := float64(usedMemoryRSS) / float64(usedMemory)
			if usedMemory == 0 {
				fragRatio = 1.0
			}
			info := fmt.Sprintf("# Memory\r\nused_memory:%d\r\nused_memory_rss:%d\r\nmem_fragmentation_ratio:%.2f\r\n", usedMemory, usedMemoryRSS, fragRatio)
			return fmt.Sprintf("$%d\r\n%s\r\n", len(info), info)
		}
		return "$0\r\n\r\n"
	default:
		return fmt.Sprintf("-ERR unknown command '%s'\r\n", args[0])
	}
}

func dbSyncDelete(key string) {
	delete(db, key)
}

func dbAsyncDelete(key string, val *Value) {
	delete(db, key)
	go func(v *Value) {
		_ = v
		runtime.GC()
		debug.FreeOSMemory()
	}(val)
}

func activeExpireCycle() {
	ticker := time.NewTicker(100 * time.Millisecond)
	for range ticker.C {
		runActiveExpireCycleIteration()
	}
}

func runActiveExpireCycleIteration() {
	effort := activeExpireEffort
	if effort < 1 {
		effort = 1
	}
	timeLimit := time.Duration(25*effort) * time.Millisecond

	startTime := time.Now()

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	usedMemory := memStats.Alloc
	usedMemoryRSS := getRSS(usedMemory)
	fragRatio := float64(usedMemoryRSS) / float64(usedMemory)
	if usedMemory == 0 {
		fragRatio = 1.0
	}

	if fragRatio > 1.5 || usedMemoryRSS > 256*1024*1024 {
		timeLimit = timeLimit / 10
		if timeLimit < time.Millisecond {
			timeLimit = time.Millisecond
		}
	}

	mu.Lock()
	keysToDelete := make([]string, 0)
	keysToAsyncDelete := make([]*Value, 0)
	keysToAsyncDeleteNames := make([]string, 0)

	count := 0
	expiredCount := 0

	for key, val := range db {
		if time.Since(startTime) > timeLimit {
			break
		}

		count++
		if !val.ExpiresAt.IsZero() && time.Now().After(val.ExpiresAt) {
			expiredCount++
			isLarge := false
			if val.Type == "hash" {
				if h, ok := val.Val.(map[string]string); ok && len(h) > 64 {
					isLarge = true
				}
			}

			if isLarge && lazyFreeLazyExpire {
				keysToAsyncDelete = append(keysToAsyncDelete, val)
				keysToAsyncDeleteNames = append(keysToAsyncDeleteNames, key)
			} else {
				keysToDelete = append(keysToDelete, key)
			}
		}

		if count >= 100 {
			if float64(expiredCount)/float64(count) < 0.25 {
				break
			}
			count = 0
			expiredCount = 0
		}
	}

	for _, key := range keysToDelete {
		dbSyncDelete(key)
	}
	for i, key := range keysToAsyncDeleteNames {
		dbAsyncDelete(key, keysToAsyncDelete[i])
	}
	mu.Unlock()

	if len(keysToDelete) > 0 {
		go func() {
			runtime.GC()
			debug.FreeOSMemory()
		}()
	}
}

func getRSS(fallback uint64) uint64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 2 {
			rssPages, err := strconv.ParseUint(fields[1], 10, 64)
			if err == nil {
				return rssPages * uint64(os.Getpagesize())
			}
		}
	}
	return fallback
}
