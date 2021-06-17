package funcs

func Contains(sa []string, i string) bool {
	for _, a := range sa {
		if a == i {
			return true
		}
	}
	return false
}

func GetRemarkByIp(ips map[string]string, mip string) string {
	for rmk,ip := range ips {
		if ip == mip {
			return rmk
		}
	}
	return ""
}