package cloudflare

import (
	"context"
	"fmt"
	"sync"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	db "github.com/reddec/filedb"
)

type Config struct {
	Key       string
	Email     string
	Domains   map[string][]string
	Ips       map[string]string
	CachePath string
}

var ctx context.Context

func init() {
	ctx, _ = context.WithCancel(context.TODO())
}

func Run(config *Config, force bool, mwg *sync.WaitGroup) {
	mwg.Add(1)
	defer mwg.Done()
	client, err := cf.New(config.Key, config.Email)
	if err != nil {
		fmt.Println(err.Error() + "could not create the Cloudflare API client")
		return
	}
	dbh := db.DB{Root: config.CachePath}
	// recordCounter := make(map[string]uint) //备注索引值

	for domain, subDomains := range config.Domains {
		if force {
			fmt.Printf("[%s] cache timeout\n", time.Now().Format("2006-01-02 15:04:05"))
			zones, err := client.ListZones(ctx, domain) // 查出根域名信息
			// fmt.Println(zones)
			if err == nil && len(zones) > 0 && domain == zones[0].Name {
				zone := zones[0]
				_ = dbh.Put(zone, domain)

				// records, err := client.DNSRecords(ctx,zone.ID,cf.DNSRecord{Type: "A"}) // 查出已有子域名
				// if err ==nil && len(records) > 0 {
				// 	for _, record := range records {
				// 		if funcs.Contains(subDomains, record.Name) {
				// 			recordCounter[record.Name+"."+domain]++
				// 			section := dbh.Section(domain, record.Name)
				// 			if remark:=funcs.GetRemarkByIp(config.Ips,record.Content);remark != ""{
				// 				_ = section.Put(remark, record)
				// 			}else {
				// 				remark := remarkNames[recordCounter[record.Name+"."+domain]-1]
				// 				fmt.Printf("[%s] Updating %s.%s with remark %s\n", time.Now().Format("2006-01-02 15:04:05"), record.Name, domain, remark) // 未有此记录,需要更新
				// 				_, _, err := client.Records.Remark(string(domainsInfo[0].ID), string(record.ID), dnspod.Record{Remark: remark})
				// 				if err != nil {
				// 					fmt.Printf("update failed: %s\n", err)
				// 				} else {

				// 					makeRecordCache(client, domainsInfo[0], record, section, remark)
				// 				}
				// 			}
				// 		}
				// 	}
				// }
				for _, subDomain := range subDomains {
					section := dbh.Section(domain, subDomain)
					section.Clean()
				}
				checkDns(subDomains, &dbh, domain, config.Ips, zone, client)
			}

		} else {
			fmt.Printf("[%s] cache used\n", time.Now().Format("2006-01-02 15:04:05"))
			zone := cf.Zone{}
			_ = dbh.Get(&zone, domain)
			checkDns(subDomains, &dbh, domain, config.Ips, zone, client)
		}
	}
}

func checkDns(subDomains []string, db *db.DB, domain string, ips map[string]string, zone cf.Zone, client *cf.API) {
	fmt.Printf("[%s] check domain %s %v\n", time.Now().Format("2006-01-02 15:04:05"), domain, subDomains)
	for _, subDomain := range subDomains {
		section := db.Section(domain, subDomain)
		createWg := sync.WaitGroup{}
		updateWg := sync.WaitGroup{}
		for remark, ip := range ips {
			var record cf.DNSRecord
			if err := section.Get(remark, &record); err != nil { // 本地无缓存 创建
				createWg.Add(1)
				go createRecord(subDomain, zone, ip, client, section, remark, domain, &createWg)
			} else if ip != record.Content && record.ID != "" { // 本地有缓存且IP已改变
				updateWg.Add(1)
				go updateRecord(subDomain, zone, ip, client, record, section, remark, &updateWg)
			} else {
				fmt.Printf("[%s] %s.%s[%s] IP无变化\n", time.Now().Format("2006-01-02 15:04:05"), subDomain, domain, remark)
			}
		}
		createWg.Wait()
		updateWg.Wait()
	}
}

func createRecord(subDomain string, zone cf.Zone, ip string, client *cf.API, section *db.Section, remark string, domain string, wg *sync.WaitGroup) {
	defer wg.Done()
	fmt.Printf("[%s] creating %s.%s[%s] , IP:%s\n", time.Now().Format("2006-01-02 15:04:05"), subDomain, zone.Name, remark, ip) // 未有此记录,需要创建
	resp, err := client.CreateDNSRecord(ctx, zone.ID, cf.DNSRecord{
		Type:    "A",
		Name:    subDomain + "." + zone.Name,
		Content: ip,
		TTL:     1,
	})
	if err != nil {
		fmt.Printf("create failed: %s\n", err)
	} else {
		newRecord := resp.Result
		_ = section.Put(remark, newRecord)
		fmt.Printf("[%s] Record Created, updating %s.%s with remark %s\n", time.Now().Format("2006-01-02 15:04:05"), newRecord.Name, domain, remark) // 未有此记录,需要更新
		makeRecordCache(newRecord, zone, section, remark)
	}
}

func updateRecord(subDomain string, zone cf.Zone, ip string, client *cf.API, record cf.DNSRecord, section *db.Section, remark string, wg *sync.WaitGroup) {
	defer wg.Done()
	fmt.Printf("[%s] Updating %s.%s[%s], IP:%s\n", time.Now().Format("2006-01-02 15:04:05"), subDomain, zone.Name, remark, ip)
	err := client.UpdateDNSRecord(ctx, record.ZoneID, record.ID, cf.DNSRecord{
		Type:    "A",
		Name:    subDomain + "." + zone.Name,
		Content: ip,
		TTL:     1,
	})
	if err != nil {
		fmt.Printf("update failed: %s\n", err)
	} else {
		record, err := client.DNSRecord(ctx, zone.ID, record.ID)
		if err != nil {
			fmt.Printf("get updated record failed: %s\n", err)
		} else {
			makeRecordCache(record, zone, section, remark)
		}
	}
}

func makeRecordCache(record cf.DNSRecord, zone cf.Zone, section *db.Section, remark string) {

	fmt.Printf("[%s] Cached %s.%s[%s]\n", time.Now().Format("2006-01-02 15:04:05"), record.Name, zone.Name, remark)
	_ = section.Put(remark, record)

}
