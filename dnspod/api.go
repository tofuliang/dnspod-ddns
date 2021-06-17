package dnspod

import (
	"ddns/funcs"
	"fmt"
	"sync"
	"time"

	db "github.com/reddec/filedb"
	"github.com/tofuliang/dnspod-go"
)

type Config struct {
	Token     string
	Domains   map[string][]string
	Ips       map[string]string
	CachePath string
}

func Run(config *Config, force bool,mwg *sync.WaitGroup) {
	mwg.Add(1)
	defer mwg.Done()

	apiToken := config.Token
	params := dnspod.CommonParams{LoginToken: apiToken, Format: "json"}
	client := dnspod.NewClient(params)
	remarks := make(map[string]string)
	remarkNames := make([]string, len(remarks))
	recordCounter := make(map[string]uint)
	dbh := db.DB{Root: config.CachePath}

	// 获取本地IP
	for remark, ip := range config.Ips {
		remarks[remark] = ip
		remarkNames = append(remarkNames, remark)
	}
	// 获取Dnspod已有配置,设置备注并缓存
	for domain, subDomains := range config.Domains {
		if force {
			fmt.Printf("[%s] cache timeout\n", time.Now().Format("2006-01-02 15:04:05"))
			domainsInfo, _, _ := client.Domains.List(&dnspod.DomainSearchParam{Keyword: domain})
			if len(domainsInfo) > 0 && domain == domainsInfo[0].Name {
				_ = dbh.Put(domainsInfo[0], domain)
				records, _, _ := client.Records.List(string(domainsInfo[0].ID), "", "A")
				if len(records) > 0 {
					wg := sync.WaitGroup{}
					for _, record := range records {
						if funcs.Contains(subDomains, record.Name) {
							recordCounter[record.Name+"."+domain]++
							section := dbh.Section(domain, record.Name)
							if record.Remark != "" {
								_ = section.Put(record.Remark, record)
							} else {
								remark := remarkNames[recordCounter[record.Name+"."+domain]-1]
								fmt.Printf("[%s] Updating %s.%s with remark %s\n", time.Now().Format("2006-01-02 15:04:05"), record.Name, domain, remark) // 未有此记录,需要更新
								_, _, err := client.Records.Remark(string(domainsInfo[0].ID), string(record.ID), dnspod.Record{Remark: remark})
								if err != nil {
									fmt.Printf("update failed: %s\n", err)
								} else {
									wg.Add(1)
									go makeRecordCache(client, domainsInfo[0], record, section, remark, &wg)
								}
							}
						}
					}
					wg.Wait()
				}
				checkDns(subDomains, &dbh, domain, remarks, domainsInfo[0], client)
			}
		} else {
			fmt.Printf("[%s] cache used\n", time.Now().Format("2006-01-02 15:04:05"))
			domainInfo := dnspod.Domain{}
			_ = dbh.Get(&domainInfo, domain)
			checkDns(subDomains, &dbh, domain, remarks, domainInfo, client)
		}
	}
}

func checkDns(subDomains []string, db *db.DB, domain string, remarks map[string]string, domainInfo dnspod.Domain, client *dnspod.Client) {
	fmt.Printf("[%s] check domain %s %v\n", time.Now().Format("2006-01-02 15:04:05"), domain, subDomains)
	for _, subDomain := range subDomains {
		section := db.Section(domain, subDomain)
		createWg := sync.WaitGroup{}
		updateWg := sync.WaitGroup{}
		for remark, ip := range remarks {
			// 本地无缓存
			var record dnspod.Record
			if err := section.Get(remark, &record); err != nil {
				createWg.Add(1)
				go createRecord(subDomain, domainInfo, ip, client, section, remark, domain, &createWg)
			} else if ip != record.Value && record.ID != "" { // 本地有缓存且IP已改变
				updateWg.Add(1)
				go updateRecord(subDomain, domainInfo, ip, client, record, section, remark, &updateWg)
			} else {
				fmt.Printf("[%s] %s.%s[%s] IP无变化\n", time.Now().Format("2006-01-02 15:04:05"), subDomain, domain, remark)
			}
		}
		createWg.Wait()
		updateWg.Wait()
	}
}

func updateRecord(subDomain string, domainInfo dnspod.Domain, ip string, client *dnspod.Client, record dnspod.Record, section *db.Section, remark string, wg *sync.WaitGroup) {
	defer wg.Done()
	fmt.Printf("[%s] Updating %s.%s[%s], IP:%s\n", time.Now().Format("2006-01-02 15:04:05"), subDomain, domainInfo.Name, remark, ip)
	_, _, err := client.Records.Update(string(domainInfo.ID), string(record.ID), dnspod.Record{Name: subDomain, Type: "A", Line: "默认", Value: ip})
	if err != nil {
		fmt.Printf("update failed: %s\n", err)
	} else {
		makeRecordCache(client, domainInfo, record, section, remark, nil)
	}
}

func createRecord(subDomain string, domainInfo dnspod.Domain, ip string, client *dnspod.Client, section *db.Section, remark string, domain string, wg *sync.WaitGroup) {
	defer wg.Done()
	fmt.Printf("[%s] creating %s.%s[%s] , IP:%s\n", time.Now().Format("2006-01-02 15:04:05"), subDomain, domainInfo.Name, remark, ip) // 未有此记录,需要创建
	newRecord, _, err := client.Records.Create(string(domainInfo.ID), dnspod.Record{Name: subDomain, Type: "A", Line: "默认", Value: ip})
	if err != nil {
		fmt.Printf("create failed: %s\n", err)
	} else {
		_ = section.Put(remark, newRecord)
		fmt.Printf("[%s] Record Created, updating %s.%s with remark %s\n", time.Now().Format("2006-01-02 15:04:05"), newRecord.Name, domain, remark) // 未有此记录,需要更新
		_, _, err := client.Records.Remark(string(domainInfo.ID), string(newRecord.ID), dnspod.Record{Remark: remark})
		if err != nil {
			fmt.Printf("update record failed: %s\n", err)
		} else {
			makeRecordCache(client, domainInfo, newRecord, section, remark, nil)
		}
	}
}

func makeRecordCache(client *dnspod.Client, domainInfo dnspod.Domain, record dnspod.Record, section *db.Section, remark string, wg *sync.WaitGroup) {
	if wg != nil {
		fmt.Printf("[%s] defer wg.Done()\n", time.Now().Format("2006-01-02 15:04:05"))
		defer wg.Done()
	}
	// time.Sleep(time.Duration(5) * time.Second)
	recordInfo, _, err := client.Records.Get(string(domainInfo.ID), string(record.ID))
	if err != nil {
		fmt.Printf("query record failed: %s\n", err)
	} else {
		fmt.Printf("[%s] Cached %s.%s[%s]\n", time.Now().Format("2006-01-02 15:04:05"), record.Name, domainInfo.Name, remark)

		_ = section.Put(remark, recordInfo)
	}
}
