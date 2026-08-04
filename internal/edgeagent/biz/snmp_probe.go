package biz

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
	"github.com/ongridio/ongrid/internal/pkg/tunnel"
)

const (
	oidSysName        = "1.3.6.1.2.1.1.5.0"
	oidSysDescription = "1.3.6.1.2.1.1.1.0"
	oidSysObjectID    = "1.3.6.1.2.1.1.2.0"
	oidIfDescr        = "1.3.6.1.2.1.2.2.1.2"
	oidIfType         = "1.3.6.1.2.1.2.2.1.3"
	oidIfPhysAddress  = "1.3.6.1.2.1.2.2.1.6"
	oidIfAdminStatus  = "1.3.6.1.2.1.2.2.1.7"
	oidIfOperStatus   = "1.3.6.1.2.1.2.2.1.8"
	oidIfName         = "1.3.6.1.2.1.31.1.1.1.1"
	oidIfAlias        = "1.3.6.1.2.1.31.1.1.1.18"
	maxSNMPInterfaces = 512
)

// ProbeNetworkSNMP performs a bounded read-only identity probe. It deliberately
// starts with system OIDs; interface and LLDP enrichment can be added after
// the device has passed this admission gate.
func ProbeNetworkSNMP(ctx context.Context, req tunnel.ProbeNetworkSNMPRequest) tunnel.ProbeNetworkSNMPResponse {
	address := strings.TrimSpace(req.Address)
	if address == "" {
		return tunnel.ProbeNetworkSNMPResponse{Error: "SNMP address is required"}
	}
	if req.Port == 0 {
		req.Port = 161
	}
	timeout := 3 * time.Second
	if req.TimeoutMilliseconds > 0 && req.TimeoutMilliseconds <= 15000 {
		timeout = time.Duration(req.TimeoutMilliseconds) * time.Millisecond
	}
	retries := req.Retries
	if retries < 0 || retries > 3 {
		retries = 1
	}

	version := strings.ToLower(strings.TrimSpace(req.Version))
	params := &gosnmp.GoSNMP{
		Target:         address,
		Port:           req.Port,
		Timeout:        timeout,
		Retries:        retries,
		MaxOids:        3,
		MaxRepetitions: 25,
		Version:        gosnmp.Version2c,
		Community:      req.Community,
		Context:        ctx,
	}
	if version == "v3" {
		security, flags, err := usmSecurity(req)
		if err != nil {
			return tunnel.ProbeNetworkSNMPResponse{Error: err.Error()}
		}
		params.Version = gosnmp.Version3
		params.SecurityParameters = security
		params.MsgFlags = flags
	} else if version != "v2c" && version != "" {
		return tunnel.ProbeNetworkSNMPResponse{Error: "SNMP version must be v2c or v3"}
	}
	if version == "v2c" && strings.TrimSpace(req.Community) == "" {
		return tunnel.ProbeNetworkSNMPResponse{Error: "SNMP community is required for v2c"}
	}

	if err := ctx.Err(); err != nil {
		return tunnel.ProbeNetworkSNMPResponse{Error: err.Error()}
	}
	if err := params.Connect(); err != nil {
		return tunnel.ProbeNetworkSNMPResponse{Error: fmt.Sprintf("SNMP connect: %v", err)}
	}
	defer params.Conn.Close()
	result, err := params.Get([]string{oidSysName, oidSysDescription, oidSysObjectID})
	if err != nil {
		return tunnel.ProbeNetworkSNMPResponse{Error: fmt.Sprintf("SNMP get: %v", err)}
	}
	response := tunnel.ProbeNetworkSNMPResponse{OK: true, IPAddress: address}
	for _, pdu := range result.Variables {
		value := snmpValue(pdu.Value)
		switch pdu.Name {
		case "." + oidSysName:
			response.SysName = value
		case "." + oidSysDescription:
			response.SysDescription = value
		case "." + oidSysObjectID:
			response.SysObjectID = value
		}
	}
	response.Interfaces = collectSNMPInterfaces(params)
	return response
}

var errInterfaceLimit = errors.New("SNMP interface limit reached")

func collectSNMPInterfaces(params *gosnmp.GoSNMP) []tunnel.NetworkInterfaceReport {
	rows := make(map[int]*tunnel.NetworkInterfaceReport)
	get := func(index int) *tunnel.NetworkInterfaceReport {
		row := rows[index]
		if row == nil {
			row = &tunnel.NetworkInterfaceReport{IfIndex: index}
			rows[index] = row
		}
		return row
	}
	walk := func(oid string, apply func(*tunnel.NetworkInterfaceReport, gosnmp.SnmpPDU)) {
		err := params.BulkWalk(oid, func(pdu gosnmp.SnmpPDU) error {
			index, ok := oidIndex(pdu.Name, oid)
			if !ok {
				return nil
			}
			if _, exists := rows[index]; !exists && len(rows) >= maxSNMPInterfaces {
				return errInterfaceLimit
			}
			apply(get(index), pdu)
			return nil
		})
		if err != nil && !errors.Is(err, errInterfaceLimit) {
			return
		}
	}

	walk(oidIfDescr, func(row *tunnel.NetworkInterfaceReport, pdu gosnmp.SnmpPDU) { row.Name = snmpValue(pdu.Value) })
	walk(oidIfName, func(row *tunnel.NetworkInterfaceReport, pdu gosnmp.SnmpPDU) {
		if name := snmpValue(pdu.Value); name != "" {
			row.Name = name
		}
	})
	walk(oidIfAlias, func(row *tunnel.NetworkInterfaceReport, pdu gosnmp.SnmpPDU) { row.Description = snmpValue(pdu.Value) })
	walk(oidIfType, func(row *tunnel.NetworkInterfaceReport, pdu gosnmp.SnmpPDU) {
		row.InterfaceKind = snmpInterfaceKind(snmpInt(pdu.Value))
	})
	walk(oidIfPhysAddress, func(row *tunnel.NetworkInterfaceReport, pdu gosnmp.SnmpPDU) {
		if value, ok := pdu.Value.([]byte); ok && len(value) > 0 {
			row.MAC = net.HardwareAddr(value).String()
		}
	})
	walk(oidIfAdminStatus, func(row *tunnel.NetworkInterfaceReport, pdu gosnmp.SnmpPDU) {
		row.AdminStatus = snmpInterfaceStatus(snmpInt(pdu.Value))
	})
	walk(oidIfOperStatus, func(row *tunnel.NetworkInterfaceReport, pdu gosnmp.SnmpPDU) {
		row.OperStatus = snmpInterfaceStatus(snmpInt(pdu.Value))
	})

	indexes := make([]int, 0, len(rows))
	for index := range rows {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	interfaces := make([]tunnel.NetworkInterfaceReport, 0, len(indexes))
	for _, index := range indexes {
		interfaces = append(interfaces, *rows[index])
	}
	return interfaces
}

func oidIndex(name, root string) (int, bool) {
	normalizedName := strings.TrimPrefix(name, ".")
	normalizedRoot := strings.TrimPrefix(root, ".")
	prefix := normalizedRoot + "."
	if !strings.HasPrefix(normalizedName, prefix) {
		return 0, false
	}
	suffix := strings.TrimPrefix(normalizedName, prefix)
	if strings.Contains(suffix, ".") {
		return 0, false
	}
	index, err := strconv.Atoi(suffix)
	return index, err == nil && index > 0
}

func snmpInt(value any) int {
	integer := gosnmp.ToBigInt(value)
	if integer == nil || !integer.IsInt64() {
		return 0
	}
	return int(integer.Int64())
}

func snmpInterfaceStatus(value int) string {
	switch value {
	case 1:
		return "up"
	case 2:
		return "down"
	case 3:
		return "testing"
	default:
		return "unknown"
	}
}

func snmpInterfaceKind(value int) string {
	switch value {
	case 6:
		return "ethernet"
	case 24:
		return "loopback"
	case 53:
		return "virtual"
	case 131:
		return "tunnel"
	case 161:
		return "lag"
	default:
		if value <= 0 {
			return "unknown"
		}
		return fmt.Sprintf("ifType %d", value)
	}
}

func usmSecurity(req tunnel.ProbeNetworkSNMPRequest) (*gosnmp.UsmSecurityParameters, gosnmp.SnmpV3MsgFlags, error) {
	if strings.TrimSpace(req.Username) == "" {
		return nil, gosnmp.NoAuthNoPriv, fmt.Errorf("SNMP username is required for v3")
	}
	security := &gosnmp.UsmSecurityParameters{UserName: req.Username}
	flags := gosnmp.NoAuthNoPriv
	switch strings.ToLower(req.AuthProtocol) {
	case "", "none", "noauth":
		security.AuthenticationProtocol = gosnmp.NoAuth
	case "md5":
		security.AuthenticationProtocol = gosnmp.MD5
		security.AuthenticationPassphrase = req.AuthSecret
		flags = gosnmp.AuthNoPriv
	case "sha", "sha1":
		security.AuthenticationProtocol = gosnmp.SHA
		security.AuthenticationPassphrase = req.AuthSecret
		flags = gosnmp.AuthNoPriv
	default:
		return nil, gosnmp.NoAuthNoPriv, fmt.Errorf("unsupported SNMP auth protocol %q", req.AuthProtocol)
	}
	switch strings.ToLower(req.PrivacyProtocol) {
	case "", "none", "nopriv":
		security.PrivacyProtocol = gosnmp.NoPriv
	case "des":
		security.PrivacyProtocol = gosnmp.DES
		security.PrivacyPassphrase = req.PrivacySecret
		flags = gosnmp.AuthPriv
	case "aes", "aes128":
		security.PrivacyProtocol = gosnmp.AES
		security.PrivacyPassphrase = req.PrivacySecret
		flags = gosnmp.AuthPriv
	default:
		return nil, gosnmp.NoAuthNoPriv, fmt.Errorf("unsupported SNMP privacy protocol %q", req.PrivacyProtocol)
	}
	return security, flags, nil
}

func snmpValue(value any) string {
	switch v := value.(type) {
	case []byte:
		return strings.TrimSpace(string(v))
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}
