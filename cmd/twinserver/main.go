package main

import (
	"flag"
	"log"
	"net/http"

	"twindemo/internal/server"
	"twindemo/internal/twin"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	db := flag.String("db", "data/twin.json", "状态持久化文件")
	flag.Parse()

	store, err := twin.NewStore(*db)
	if err != nil {
		log.Fatalf("加载状态失败: %v", err)
	}
	srv := server.New(store)
	log.Printf("设备状态孪生服务监听 %s，状态文件 %s", *addr, *db)
	if err := http.ListenAndServe(*addr, srv.Routes()); err != nil {
		log.Fatal(err)
	}
}
