package main

import (
	"ddns/cloudflare"
	"ddns/dnspod"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	db "github.com/reddec/filedb"
)

type Config struct {
	Dnspod struct {
		Token   string              `json:"token"`
		Domains map[string][]string `json:"domains"`
	} `json:"dnspod"`
	Cloudflare struct {
		Key     string              `json:"key"`
		Email   string              `json:"email"`
		Domains map[string][]string `json:"domains"`
	} `json:"cloudflare"`
	Ips map[string]string `json:"ips"`
}

var (
	confFilePath  = flag.String("c", "dns.json", "配置文件路径")
	cachePath     = flag.String("d", "/tmp/dns.Cache", "缓存文件路径")
	cacheTime     int64
	config        Config
	configChanged = false
)

func main() {
	flag.Parse()
	fmt.Printf("[%s] starting ...\n", time.Now().Format("2006-01-02 15:04:05"))
	configFile, err := os.Open(*confFilePath)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = configFile.Close() }()
	byteValue, _ := ioutil.ReadAll(configFile)

	file, err := os.Stat(*confFilePath)
	if err != nil {
		fmt.Println(err)
	}
	modTime := file.ModTime().Unix()
	_ = json.Unmarshal(byteValue, &config)
	dbh := db.DB{Root: *cachePath}
	remarks := make(map[string]string)

	// 获取本地IP
	for remark, cmd := range config.Ips {
		out, err := exec.Command("sh", "-c", cmd).Output()
		if err != nil {
			fmt.Printf("%s", err)
		}
		remarks[remark] = strings.TrimSuffix(string(out), "\n")
		fmt.Printf("[%s] got remark: %s, IP: %s", time.Now().Format("2006-01-02 15:04:05"), remark, out)
	}

	_ = dbh.Get(&cacheTime, "cacheTime")
	if cacheTime < modTime {
		configChanged = true
	}
	mwg := sync.WaitGroup{}

	dnspod.Run(&dnspod.Config{
		Token:     config.Dnspod.Token,
		Domains:   config.Dnspod.Domains,
		Ips:       remarks,
		CachePath: *cachePath,
	}, configChanged, &mwg)

	cloudflare.Run(&cloudflare.Config{
		Key:       config.Cloudflare.Key,
		Email:     config.Cloudflare.Email,
		Domains:   config.Cloudflare.Domains,
		Ips:       remarks,
		CachePath: *cachePath,
	}, configChanged, &mwg)

	mwg.Wait()
	_ = dbh.Put(time.Now().Unix(), "cacheTime")
	fmt.Printf("[%s] end\n", time.Now().Format("2006-01-02 15:04:05"))
}
