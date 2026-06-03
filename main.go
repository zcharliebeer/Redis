package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"strings"
)

func main() {
	fmt.Println("Optimizing Redis memory usage and active expiration cycle...")

	// 1. Modify src/server.h
	serverHPath := "src/server.h"
	if content, err := ioutil.ReadFile(serverHPath); err == nil {
		sContent := string(content)
		if !strings.Contains(sContent, "stat_expire_time_spent") {
			target := "long long stat_expiredkeys;"
			replacement := "long long stat_expiredkeys;\n    long long stat_expire_time_spent;\n    long long stat_expire_memory_freed;"
			sContent = strings.Replace(sContent, target, replacement, 1)
			if err := ioutil.WriteFile(serverHPath, []byte(sContent), 0644); err != nil {
				log.Printf("Failed to write src/server.h: %v", err)
			} else {
				fmt.Println("Successfully modified src/server.h")
			}
		}
	} else {
		log.Printf("Failed to read src/server.h: %v", err)
	}

	// 2. Modify src/expire.c
	expireCPath := "src/expire.c"
	if content, err := ioutil.ReadFile(expireCPath); err == nil {
		sContent := string(content)
		modified := false

		// Insert variables at the start of activeExpireCycle
		if !strings.Contains(sContent, "keys_expired_this_cycle") {
			target := "void activeExpireCycle(int type) {"
			replacement := "void activeExpireCycle(int type) {\n    long long start_time = ustime();\n    size_t mem_before = zmalloc_used_memory();\n    long long keys_expired_this_cycle = 0;"
			sContent = strings.Replace(sContent, target, replacement, 1)
			modified = true
		}

		// Increment keys_expired_this_cycle
		if strings.Contains(sContent, "if (activeExpireCycleTryExpire(db,de,now)) expired++;") {
			sContent = strings.Replace(sContent,
				"if (activeExpireCycleTryExpire(db,de,now)) expired++;",
				"if (activeExpireCycleTryExpire(db,de,now)) { expired++; keys_expired_this_cycle++; }",
				-1)
			modified = true
		} else if strings.Contains(sContent, "if (activeExpireCycleTryExpire(db,de,now))\n                expired++;") {
			sContent = strings.Replace(sContent,
				"if (activeExpireCycleTryExpire(db,de,now))\n                expired++;",
				"if (activeExpireCycleTryExpire(db,de,now)) {\n                expired++;\n                keys_expired_this_cycle++;\n            }",
				-1)
			modified = true
		}

		// Replace the periodic check
		periodicCheckTarget := `            if (elapsed > timelimit) {
                timelimit_exit = 1;
                break;
            }`
		if strings.Contains(sContent, periodicCheckTarget) && !strings.Contains(sContent, "aof_buf_size") {
			periodicCheckReplacement := `            if (elapsed > timelimit) {
                timelimit_exit = 1;
                break;
            }
            
            /* Check replication and AOF buffer sizes to prevent memory spikes */
            size_t aof_buf_size = server.aof_state != AOF_OFF ? sdslen(server.aof_buf) : 0;
            size_t repl_buf_size = 0;
            listIter li;
            listNode *ln;
            listRewind(server.slaves, &li);
            while((ln = listNext(&li))) {
                client *slave = listNodeValue(ln);
                repl_buf_size += slave->reply_bytes + slave->bufpos;
            }
            
            if (aof_buf_size > 16*1024*1024) {
                flushAppendOnlyFile(0);
                aof_buf_size = sdslen(server.aof_buf);
            }
            if (aof_buf_size > 16*1024*1024 || repl_buf_size > 16*1024*1024) {
                timelimit_exit = 1;
                break;
            }
            
            /* Respect maxmemory limits during active expiration */
            if (server.maxmemory > 0 && zmalloc_used_memory() > server.maxmemory) {
                timelimit_exit = 1;
                break;
            }`
			sContent = strings.Replace(sContent, periodicCheckTarget, periodicCheckReplacement, 1)
			modified = true
		}

		// Insert metrics update and zmalloc_purge at the end of activeExpireCycle
		if strings.Contains(sContent, `latencyAddSampleIfNeeded("expire-cycle"`) && !strings.Contains(sContent, "stat_expire_time_spent") {
			endReplacement := `long long duration = ustime() - start_time;
    size_t mem_after = zmalloc_used_memory();
    long long mem_freed = (mem_before > mem_after) ? (mem_before - mem_after) : 0;
    server.stat_expire_time_spent += duration;
    server.stat_expire_memory_freed += mem_freed;

    if (keys_expired_this_cycle > 0) {
        #ifdef USE_JEMALLOC
        size_t rss = zmalloc_get_rss();
        double frag = zmalloc_get_fragmentation_ratio(rss);
        if (frag > 1.4) {
            zmalloc_purge();
        }
        #endif
    }

    latencyAddSampleIfNeeded("expire-cycle"`
			sContent = strings.Replace(sContent, `latencyAddSampleIfNeeded("expire-cycle"`, endReplacement, 1)
			modified = true
		}

		if modified {
			if err := ioutil.WriteFile(expireCPath, []byte(sContent), 0644); err != nil {
				log.Printf("Failed to write src/expire.c: %v", err)
			} else {
				fmt.Println("Successfully modified src/expire.c")
			}
		}
	} else {
		log.Printf("Failed to read src/expire.c: %v", err)
	}

	// 3. Modify tests/unit/expire.tcl
	expireTclPath := "tests/unit/expire.tcl"
	if content, err := ioutil.ReadFile(expireTclPath); err == nil {
		sContent := string(content)
		testCase := `
start_server {tags {"expire"}} {
    test {Active expiration respects maxmemory and reclaims memory under load} {
        r config set maxmemory 50mb
        r config set maxmemory-policy noeviction
        r config set lazyfree-lazy-expire yes
        
        set val [string repeat "x" 1000]
        for {set j 0} {$j < 20000} {incr j} {
            r setex "key:$j" 2 $val
        }
        
        after 2500
        catch {r config set active-expire-effort 10}
        after 1000
        
        set mem [s used_memory]
        assert {$mem < 15000000}
        assert {[r ping] eq "PONG"}
    }
}
`
		if !strings.Contains(sContent, "Active expiration respects maxmemory and reclaims memory under load") {
			sContent += testCase
			if err := ioutil.WriteFile(expireTclPath, []byte(sContent), 0644); err != nil {
				log.Printf("Failed to write tests/unit/expire.tcl: %v", err)
			} else {
				fmt.Println("Successfully modified tests/unit/expire.tcl")
			}
		}
	} else {
		log.Printf("Failed to read tests/unit/expire.tcl: %v", err)
	}
}
