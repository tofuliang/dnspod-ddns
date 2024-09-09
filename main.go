package main

import (
	"crypto/tls"
	"dnspod-ddns/db"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	tencentErrors "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/errors"

	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/regions"
	dnspod "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/dnspod/v20210323"
)

type Config struct {
	SecretId   string              `json:"secretId"`
	SecretKey  string              `json:"secretKey"`
	HttpRecord bool                `json:"httpRecord"`
	H3Port     json.Number         `json:"h3Port"`
	Domains    map[string][]string `json:"domains"`
	Ips        map[string]string   `json:"ips"`
}

func contains(sa []string, i string) bool {
	for _, a := range sa {
		_, a = parseSubdomain(a)
		if a == i {
			return true
		}
	}
	return false
}

var confFilePath = flag.String("c", "dns.json", "配置文件路径")
var cachePath = flag.String("d", "/tmp/dns.Cache", "缓存文件路径")
var config Config
var rateLimiter = make(chan struct{}, 80)

func initRateLimiter() {
	ticker := time.NewTicker(time.Second)
	go func() {
		for range ticker.C {
			for i := 0; i < 80; i++ {
				select {
				case rateLimiter <- struct{}{}:
				default:
					// 如果通道已满，不做任何操作
				}
			}
		}
	}()
}

func main() {
	initRateLimiter()
	flag.Parse()
	fmt.Printf("[%s] starting ...\n", time.Now().Format("2006-01-02 15:04:05"))
	configFile, err := os.Open(*confFilePath)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = configFile.Close() }()
	byteValue, _ := io.ReadAll(configFile)

	file, err := os.Stat(*confFilePath)
	if err != nil {
		fmt.Println(err)
	}
	modTime := file.ModTime().Unix()

	_ = json.Unmarshal(byteValue, &config)

	dbh := db.DB{Root: *cachePath}

	credential := common.NewCredential(
		config.SecretId,
		config.SecretKey,
	)
	client, _ := dnspod.NewClient(credential, regions.Guangzhou, profile.NewClientProfile())
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client.WithHttpTransport(tr)
	remarks := make(map[string]string)
	remarkNames := make([]string, len(remarks))
	recordCounter := make(map[string]uint)
	// 获取本地IP
	for remark, cmd := range config.Ips {
		out, err := exec.Command("sh", "-c", cmd).Output()
		if err != nil {
			fmt.Printf("%s", err)
		}
		remarks[remark] = strings.TrimSuffix(string(out), "\n")
		remarkNames = append(remarkNames, remark)
		fmt.Printf("[%s] got remark: %s, IP: %s", time.Now().Format("2006-01-02 15:04:05"), remark, out)
	}
	var cacheTime int64
	_ = dbh.Get(&cacheTime, "cacheTime")
	// 获取Dnspod已有配置,设置备注并缓存
	var success = true
	var limit uint64 = 3000
	for domain, subDomains := range config.Domains {
		s := strings.Split(subDomains[0], "#")
		section := dbh.Section(domain, s[0])
		if len(section.List()) < 1 || cacheTime < modTime {
			fmt.Printf("[%s] cache timeout\n", time.Now().Format("2006-01-02 15:04:05"))
			describeDomainRequest := dnspod.NewDescribeDomainRequest()
			describeDomainRequest.Domain = &domain
			<-rateLimiter
			describeDomainResponse, err := client.DescribeDomain(describeDomainRequest)
			var tencentCloudSDKError *tencentErrors.TencentCloudSDKError
			if errors.As(err, &tencentCloudSDKError) {
				fmt.Printf("An API error has returned: %s", err)
				continue
			}
			if describeDomainResponse == nil || describeDomainResponse.Response == nil || describeDomainResponse.Response.DomainInfo == nil {
				fmt.Printf("Empty DomainInfo returned: %v", describeDomainResponse)
				continue
			}
			domainInfo := describeDomainResponse.Response.DomainInfo

			_ = dbh.Put(domainInfo, domain)
			describeRecordListRequest := dnspod.NewDescribeRecordListRequest()
			describeRecordListRequest.Domain = &domain
			describeRecordListRequest.Limit = &limit
			<-rateLimiter
			describeRecordListResponse, err := client.DescribeRecordList(describeRecordListRequest)
			if errors.As(err, &tencentCloudSDKError) {
				fmt.Printf("An API error has returned: %s", err)
				continue
			}
			if describeRecordListResponse == nil || describeRecordListResponse.Response == nil || len(describeRecordListResponse.Response.RecordList) < 1 {
				fmt.Printf("Empty RecordList returned: %v", describeRecordListResponse)
				continue
			}

			records := describeRecordListResponse.Response.RecordList

			wg := sync.WaitGroup{}
			for _, record := range records {
				if strings.ToUpper(*record.Status) == `ENABLE` && contains(subDomains, *record.Name) {
					recordId := *record.Type + "-" + *record.Remark
					recordCounter[recordId+"."+domain]++
					section := dbh.Section(domain, *record.Name)
					if *record.Remark != "" {
						// 更新记录缓存
						wg.Add(1)
						go makeRecordCache(client, domainInfo, record.RecordId, section, record.Remark, &wg)
					} else {
						remark := remarkNames[recordCounter[recordId+"."+domain]-1]
						fmt.Printf("[%s] Updating %s.%s with remark %s\n", time.Now().Format("2006-01-02 15:04:05"), *record.Name, domain, remark) // 未有此记录,需要更新
						modifyRecordRemarkRequest := dnspod.NewModifyRecordRemarkRequest()
						modifyRecordRemarkRequest.Domain = &domain
						modifyRecordRemarkRequest.Remark = &remark
						modifyRecordRemarkRequest.RecordId = record.RecordId
						<-rateLimiter
						_, err := client.ModifyRecordRemark(modifyRecordRemarkRequest)
						if err != nil {
							fmt.Printf("update failed: %s\n", err)
						} else {
							wg.Add(1)
							go makeRecordCache(client, domainInfo, record.RecordId, section, &remark, &wg)
						}
					}
				}
			}
			wg.Wait()

			checkDns(subDomains, &dbh, domain, remarks, domainInfo, client)
			//domainsInfo, _, _ := client.Domains.List(&dnspod.DomainSearchParam{Keyword: domain})
			//if len(domainsInfo) > 0 && domain == domainsInfo[0].Name {
			//	_ = dbh.Put(domainsInfo[0], domain)
			//	records, _, _ := client.Records.List(string(domainsInfo[0].ID), "", "A")
			//	if len(records) > 0 {
			//
			//	}
			//
			//}
		} else {
			fmt.Printf("[%s] cache used\n", time.Now().Format("2006-01-02 15:04:05"))
			domainInfo := &dnspod.DomainInfo{}
			err = dbh.Get(&domainInfo, domain)
			if err != nil {
				fmt.Printf("[%s] get %s info failed\n", time.Now().Format("2006-01-02 15:04:05"), domain)
				success = false
				continue
			}
			checkDns(subDomains, &dbh, domain, remarks, domainInfo, client)
		}
	}
	// success and put cacheTime
	if success {
		_ = dbh.Put(time.Now().Unix(), "cacheTime")
	} else {
		_ = dbh.Put(0, "cacheTime")
	}
	fmt.Printf("[%s] end\n", time.Now().Format("2006-01-02 15:04:05"))
}

func checkDns(subDomains []string, db *db.DB, domain string, remarks map[string]string, domainInfo *dnspod.DomainInfo, client *dnspod.Client) {
	fmt.Printf("[%s] check domain %s %v\n", time.Now().Format("2006-01-02 15:04:05"), domain, subDomains)
	for _, subDomain := range subDomains {
		rmk, subDomain := parseSubdomain(subDomain)
		section := db.Section(domain, subDomain)
		createWg := sync.WaitGroup{}
		updateWg := sync.WaitGroup{}
		for remark, ip := range remarks {
			if len(rmk) > 0 && rmk != remark {
				continue
			}
			// 本地无缓存
			var record dnspod.RecordInfo
			lSubDomain := subDomain
			lIp := ip
			lRemark := remark
			dnsType := getDNSType(ip)
			if config.HttpRecord && dnsType == "A" {
				var record dnspod.RecordInfo
				lSubDomain := subDomain
				lIp := ip
				lRemark := remark
				recordId := "HTTPS" + "-" + remark
				if err := section.Get(recordId, &record); err != nil {
					createWg.Add(1)
					go createHttpsRecord(&lSubDomain, domainInfo, &lIp, client, section, &lRemark, &createWg)
				} else if !strings.Contains(*record.Value, lIp) && record.Id != nil { // 本地有缓存且IP已改变
					updateWg.Add(1)
					go updateHttpsRecord(&lSubDomain, domainInfo, &lIp, client, &record, section, &lRemark, &updateWg)
				}
			}
			recordId := dnsType + "-" + remark
			if err := section.Get(recordId, &record); err != nil {
				createWg.Add(1)
				go createRecord(&lSubDomain, domainInfo, &lIp, client, section, &lRemark, &createWg)
			} else if lIp != *record.Value && record.Id != nil { // 本地有缓存且IP已改变
				updateWg.Add(1)
				go updateRecord(&lSubDomain, domainInfo, &lIp, client, &record, section, &lRemark, &updateWg)
			} else {
				fmt.Printf("[%s] %s.%s[%s] IP无变化\n", time.Now().Format("2006-01-02 15:04:05"), lSubDomain, domain, lRemark)
			}
		}
		createWg.Wait()
		updateWg.Wait()
	}
}

func updateRecord(subDomain *string, domainInfo *dnspod.DomainInfo, ip *string, client *dnspod.Client, record *dnspod.RecordInfo, section *db.Section, remark *string, wg *sync.WaitGroup) {
	defer wg.Done()
	fmt.Printf("[%s] Updating %s.%s[%s], IP:%s\n", time.Now().Format("2006-01-02 15:04:05"), *subDomain, *domainInfo.Domain, *remark, *ip)
	modifyRecordRequest := dnspod.NewModifyRecordRequest()
	modifyRecordRequest.Domain = domainInfo.Domain
	modifyRecordRequest.RecordType = record.RecordType
	modifyRecordRequest.RecordLine = record.RecordLine
	modifyRecordRequest.Value = ip
	modifyRecordRequest.RecordId = record.Id
	modifyRecordRequest.SubDomain = subDomain
	modifyRecordRequest.RecordLineId = record.RecordLineId
	modifyRecordRequest.Remark = record.Remark
	<-rateLimiter
	_, err := client.ModifyRecord(modifyRecordRequest)
	if err != nil {
		fmt.Printf("update failed: %s\n", err)
		req, _ := json.Marshal(modifyRecordRequest)
		fmt.Printf("modifyRecordRequest: %s\n", req)
	} else {
		makeRecordCache(client, domainInfo, record.Id, section, remark, nil)
	}
}
func updateHttpsRecord(subDomain *string, domainInfo *dnspod.DomainInfo, ip *string, client *dnspod.Client, record *dnspod.RecordInfo, section *db.Section, remark *string, wg *sync.WaitGroup) {
	defer wg.Done()

	value := fmt.Sprintf(`%s.%s. alpn="h3" ipv4hint="%s" port="%s"`, *subDomain, *domainInfo.Domain, *ip, config.H3Port)
	if *subDomain == "@" {
		value = fmt.Sprintf(`%s. alpn="h3" ipv4hint="%s" port="%s"`, *domainInfo.Domain, *ip, config.H3Port)
	}

	fmt.Printf("[%s] Updating %s.%s[%s], value:%s\n", time.Now().Format("2006-01-02 15:04:05"), *subDomain, *domainInfo.Domain, *remark, value)

	modifyRecordRequest := dnspod.NewModifyRecordRequest()
	modifyRecordRequest.Domain = domainInfo.Domain
	modifyRecordRequest.RecordType = record.RecordType
	modifyRecordRequest.RecordLine = record.RecordLine
	modifyRecordRequest.MX = record.MX
	modifyRecordRequest.Value = &value
	modifyRecordRequest.RecordId = record.Id
	modifyRecordRequest.SubDomain = subDomain
	modifyRecordRequest.RecordLineId = record.RecordLineId
	modifyRecordRequest.Remark = record.Remark
	<-rateLimiter
	_, err := client.ModifyRecord(modifyRecordRequest)
	if err != nil {
		fmt.Printf("update failed: %s\n", err)
		req, _ := json.Marshal(modifyRecordRequest)
		fmt.Printf("modifyRecordRequest: %s\n", req)
	} else {
		makeRecordCache(client, domainInfo, record.Id, section, remark, nil)
	}
}

func createRecord(subDomain *string, domainInfo *dnspod.DomainInfo, ip *string, client *dnspod.Client, section *db.Section, remark *string, wg *sync.WaitGroup) {
	defer wg.Done()
	fmt.Printf("[%s] creating %s.%s[%s] , IP:%s\n", time.Now().Format("2006-01-02 15:04:05"), *subDomain, *domainInfo.Domain, *remark, *ip) // 未有此记录,需要创建
	recordType := getDNSType(*ip)
	recordLine := "默认"
	createRecordRequest := dnspod.NewCreateRecordRequest()
	createRecordRequest.SubDomain = subDomain
	createRecordRequest.Domain = domainInfo.Domain
	createRecordRequest.RecordType = &recordType
	createRecordRequest.RecordLine = &recordLine
	createRecordRequest.Value = ip
	createRecordRequest.Remark = remark
	<-rateLimiter
	createRecordResponse, err := client.CreateRecord(createRecordRequest)
	var tencentCloudSDKError *tencentErrors.TencentCloudSDKError
	if errors.As(err, &tencentCloudSDKError) {
		fmt.Printf("An API error has returned: %s\n", err)
		req, _ := json.Marshal(createRecordRequest)
		fmt.Printf("createRecordRequest: %s\n", req)
		return
	}
	if createRecordResponse == nil || createRecordResponse.Response == nil || createRecordResponse.Response.RecordId == nil {
		fmt.Printf("Empty RecordId returned: %v", createRecordResponse)
		return
	}

	if err == nil {
		makeRecordCache(client, domainInfo, createRecordResponse.Response.RecordId, section, remark, nil)
	}
}

func createHttpsRecord(subDomain *string, domainInfo *dnspod.DomainInfo, ip *string, client *dnspod.Client, section *db.Section, remark *string, wg *sync.WaitGroup) {
	defer wg.Done()

	value := fmt.Sprintf(`%s.%s. alpn="h3" ipv4hint="%s" port="%s"`, *subDomain, *domainInfo.Domain, *ip, config.H3Port)
	if *subDomain == "@" {
		value = fmt.Sprintf(`%s. alpn="h3" ipv4hint="%s" port="%s"`, *domainInfo.Domain, *ip, config.H3Port)
	}

	fmt.Printf("[%s] creating %s.%s[%s] , value:%s\n", time.Now().Format("2006-01-02 15:04:05"), *subDomain, *domainInfo.Domain, *remark, value) // 未有此记录,需要创建

	recordType := "HTTPS"
	recordLine := "默认"
	var mx uint64 = 1
	createRecordRequest := dnspod.NewCreateRecordRequest()
	createRecordRequest.SubDomain = subDomain
	createRecordRequest.Domain = domainInfo.Domain
	createRecordRequest.RecordType = &recordType
	createRecordRequest.RecordLine = &recordLine
	createRecordRequest.Value = &value
	createRecordRequest.Remark = remark
	createRecordRequest.MX = &mx
	<-rateLimiter
	createRecordResponse, err := client.CreateRecord(createRecordRequest)
	var tencentCloudSDKError *tencentErrors.TencentCloudSDKError
	if errors.As(err, &tencentCloudSDKError) {
		fmt.Printf("An API error has returned: %s\n", err)
		req, _ := json.Marshal(createRecordRequest)
		fmt.Printf("createHttpsRecord: %s\n", req)
		return
	}
	if createRecordResponse == nil || createRecordResponse.Response == nil || createRecordResponse.Response.RecordId == nil {
		fmt.Printf("Empty RecordId returned: %v", createRecordResponse)
		return
	}

	if err == nil {
		makeRecordCache(client, domainInfo, createRecordResponse.Response.RecordId, section, remark, nil)
	}
}

func makeRecordCache(client *dnspod.Client, domainInfo *dnspod.DomainInfo, recordId *uint64, section *db.Section, remark *string, wg *sync.WaitGroup) {
	if wg != nil {
		//fmt.Printf("[%s] defer wg.Done()\n", time.Now().Format("2006-01-02 15:04:05"))
		defer wg.Done()
	}
	record, err := getRecordInfo(domainInfo.Domain, recordId, client)
	if err != nil {
		return
	}
	fmt.Printf("[%s] Cached [%s] %s.%s[%s]\n", time.Now().Format("2006-01-02 15:04:05"), *record.RecordType, *record.SubDomain, *domainInfo.Domain, *remark)
	_recordId := *record.RecordType + "-" + *remark
	_ = section.Put(_recordId, record)

}

func parseSubdomain(subDomain string) (remark, domain string) {
	remark = ""
	domain = ""
	s := strings.Split(subDomain, "#")
	if len(s) > 1 {
		domain = s[0]
		remark = s[1]
	} else {
		domain = s[0]
	}
	return
}

func getRecordInfo(domain *string, recordId *uint64, client *dnspod.Client) (*dnspod.RecordInfo, error) {
	var tencentCloudSDKError *tencentErrors.TencentCloudSDKError
	describeRecordRequest := dnspod.NewDescribeRecordRequest()
	describeRecordRequest.Domain = domain
	describeRecordRequest.RecordId = recordId
	<-rateLimiter
	describeRecordResponse, err := client.DescribeRecord(describeRecordRequest)
	if errors.As(err, &tencentCloudSDKError) {
		fmt.Printf("An API error has returned: %s", err)
		return nil, err
	}
	if describeRecordResponse == nil || describeRecordResponse.Response == nil || describeRecordResponse.Response.RecordInfo == nil {
		fmt.Printf("Empty RecordInfo returned: %v", describeRecordResponse)
		return nil, err
	}
	return describeRecordResponse.Response.RecordInfo, nil
}

func getDNSType(value string) string {
	value = strings.TrimSpace(value)
	if ip := net.ParseIP(value); ip != nil {
		if strings.Contains(value, ":") {
			return "AAAA"
		}
		return "A"
	}
	return ""
}
