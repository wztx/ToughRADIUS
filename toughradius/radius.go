package toughradius

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/panjf2000/ants/v2"
	"github.com/talkincode/toughradius/v8/app"
	"github.com/talkincode/toughradius/v8/common"
	"github.com/talkincode/toughradius/v8/common/zaplog/log"
	"github.com/talkincode/toughradius/v8/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2866"
	"layeh.com/radius/rfc2869"
)

const (
	VendorMikrotik = "14988"
	VendorIkuai    = "10055"
	VendorHuawei   = "2011"
	VendorZte      = "3902"
	VendorH3c      = "25506"
	VendorRadback  = "2352"
	VendorCisco    = "9"

	RadiusRejectDelayTimes = 7
	RadiusAuthRateInterval = 1
)

type VendorRequest struct {
	MacAddr string
	Vlanid1 int64
	Vlanid2 int64
}

type AuthRateUser struct {
	Username  string
	Starttime time.Time
}

type RadiusService struct {
	App           *app.Application
	RejectCache   *RejectCache
	AuthRateCache map[string]AuthRateUser
	TaskPool      *ants.Pool
	arclock       sync.Mutex
}

func NewRadiusService() *RadiusService {
	poolsize, err := strconv.Atoi(os.Getenv("TOUGHRADIUS_RADIUS_POOL"))
	if err != nil {
		poolsize = 1024
	}
	pool, err := ants.NewPool(poolsize)
	common.Must(err)
	s := &RadiusService{
		AuthRateCache: make(map[string]AuthRateUser),
		arclock:       sync.Mutex{},
		TaskPool:      pool,
		RejectCache: &RejectCache{
			Items: make(map[string]*RejectItem),
			Lock:  sync.Mutex{},
		}}
	return s
}

func (s *RadiusService) RADIUSSecret(ctx context.Context, remoteAddr net.Addr) ([]byte, error) {
	return []byte("mysecret"), nil
}

// GetNas 查询 NAS 设备, 优先查询IP, 然后ID
func (s *RadiusService) GetNas(ip, identifier string) (vpe *models.NetVpe, err error) {
	err = app.GDB().
		Where("ipaddr = ? or identifier = ?", ip, identifier).
		First(&vpe).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, NewAuthError(app.MetricsRadiusRejectUnauthorized,
				fmt.Sprintf("unauthorized access to device, Ip=%s, Identifier=%s, %s",
					ip, identifier, err.Error()))
		}
		return nil, err
	}
	return vpe, nil
}

// GetValidUser 获取有效用户, 初步判断用户有效性
func (s *RadiusService) GetValidUser(usernameOrMac string, macauth bool) (user *models.RadiusUser, err error) {
	if macauth {
		err = app.GDB().
			Where("mac_addr = ?", usernameOrMac).
			First(&user).Error
	} else {
		err = app.GDB().
			Where("username = ?", usernameOrMac).
			First(&user).Error
	}

	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, NewAuthError(app.MetricsRadiusRejectNotExists, "user not exists")
		}
		return nil, err
	}

	if user.Status == common.DISABLED {
		return nil, NewAuthError(app.MetricsRadiusRejectDisable, "user status is disabled")
	}

	if user.ExpireTime.Before(time.Now()) {
		return nil, NewAuthError(app.MetricsRadiusRejectExpire, "user expire")
	}
	return user, nil
}

// GetLdapServer 获取Ldap 服务节点新
func (s *RadiusService) GetLdapServer(id interface{}) (item *models.NetLdapServer, err error) {
	err = app.GDB().Where("id = ?", id).First(&item).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, NewAuthError(app.MetricsRadiusRejectLdapError, "ldap node not exists")
		}
		return nil, err
	}
	if item.Status == common.DISABLED {
		return nil, NewAuthError(app.MetricsRadiusRejectLdapError, "ldap status is disabled")
	}
	return item, nil
}

// GetUserForAcct 获取用户, 不判断用户过期等状态
func (s *RadiusService) GetUserForAcct(username string) (user *models.RadiusUser, err error) {
	err = app.GDB().
		Where("username = ?", username).
		First(&user).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, NewAuthError(app.MetricsRadiusRejectNotExists, "user not exists")
		}
		return nil, err
	}
	return user, nil
}

func (s *RadiusService) UpdateUserMac(username string, macaddr string) {
	err := app.GDB().
		Model(&models.RadiusUser{}).
		Where("username = ?", username).
		Update("mac_addr", macaddr).Error
	if err != nil {
		log.Error2("update user mac error", zap.Error(err), zap.String("namespace", "radius"))
	}
}

func (s *RadiusService) UpdateUserVlanid1(username string, vlanid1 int) {
	err := app.GDB().
		Model(&models.RadiusUser{}).
		Where("username = ?", username).
		Update("vlanid1", vlanid1).Error
	if err != nil {
		log.Error2("update user vlanid1 error", zap.Error(err), zap.String("namespace", "radius"))
	}
}

func (s *RadiusService) UpdateUserVlanid2(username string, vlanid2 int) {
	err := app.GDB().
		Model(&models.RadiusUser{}).
		Where("username = ?", username).
		Update("vlanid2", vlanid2).Error
	if err != nil {
		log.Error2("update user vlanid2 error", zap.Error(err), zap.String("namespace", "radius"))
	}
}

func (s *RadiusService) GetIntConfig(name string, defval int64) int64 {
	cval := app.GApp().GetSettingsStringValue("radius", name)
	ival, err := strconv.ParseInt(cval, 10, 64)
	if err != nil {
		return defval
	}
	return ival
}

func (s *RadiusService) GetStringConfig(name string, defval string) string {
	val := app.GApp().GetSettingsStringValue("radius", name)
	if val == "" {
		return defval
	}
	return val
}

func GetNetRadiusOnlineFromRequest(r *radius.Request, vr *VendorRequest, vpe *models.NetVpe, nasrip string) models.RadiusOnline {
	acctInputOctets := int(rfc2866.AcctInputOctets_Get(r.Packet))
	acctInputGigawords := int(rfc2869.AcctInputGigawords_Get(r.Packet))
	acctOutputOctets := int(rfc2866.AcctOutputOctets_Get(r.Packet))
	acctOutputGigawords := int(rfc2869.AcctOutputGigawords_Get(r.Packet))

	getAcctStartTime := func(sessionTime int) time.Time {
		m, _ := time.ParseDuration(fmt.Sprintf("-%ds", sessionTime))
		return time.Now().Add(m)
	}
	return models.RadiusOnline{
		ID:                0,
		Username:          rfc2865.UserName_GetString(r.Packet),
		NasId:             common.IfEmptyStr(rfc2865.NASIdentifier_GetString(r.Packet), common.NA),
		NasAddr:           vpe.Ipaddr,
		NasPaddr:          nasrip,
		SessionTimeout:    int(rfc2865.SessionTimeout_Get(r.Packet)),
		FramedIpaddr:      common.IfEmptyStr(rfc2865.FramedIPAddress_Get(r.Packet).String(), common.NA),
		FramedNetmask:     common.IfEmptyStr(rfc2865.FramedIPNetmask_Get(r.Packet).String(), common.NA),
		MacAddr:           common.IfEmptyStr(vr.MacAddr, common.NA),
		NasPort:           0,
		NasClass:          common.NA,
		NasPortId:         common.IfEmptyStr(rfc2869.NASPortID_GetString(r.Packet), common.NA),
		NasPortType:       0,
		ServiceType:       0,
		AcctSessionId:     rfc2866.AcctSessionID_GetString(r.Packet),
		AcctSessionTime:   int(rfc2866.AcctSessionTime_Get(r.Packet)),
		AcctInputTotal:    int64(acctInputOctets) + int64(acctInputGigawords)*4*1024*1024*1024,
		AcctOutputTotal:   int64(acctOutputOctets) + int64(acctOutputGigawords)*4*1024*1024*1024,
		AcctInputPackets:  int(rfc2866.AcctInputPackets_Get(r.Packet)),
		AcctOutputPackets: int(rfc2866.AcctInputPackets_Get(r.Packet)),
		AcctStartTime:     getAcctStartTime(int(rfc2866.AcctSessionTime_Get(r.Packet))),
		LastUpdate:        time.Now(),
	}

}

// CheckAuthRateLimit
// Authentication frequency detection, each user can only authenticate once every few seconds
func (s *RadiusService) CheckAuthRateLimit(username string) error {
	s.arclock.Lock()
	defer s.arclock.Unlock()
	val, ok := s.AuthRateCache[username]
	if ok {
		if time.Now().Before(val.Starttime.Add(time.Duration(RadiusAuthRateInterval) * time.Second)) {
			return NewAuthError(app.MetricsRadiusRejectLimit, "there is a authentication still in process")
		}
		delete(s.AuthRateCache, username)
	}
	s.AuthRateCache[username] = AuthRateUser{
		Username:  username,
		Starttime: time.Now(),
	}
	return nil
}

func (s *RadiusService) ReleaseAuthRateLimit(username string) {
	s.arclock.Lock()
	defer s.arclock.Unlock()
	delete(s.AuthRateCache, username)
}

func (s *RadiusService) AddRadiusOnline(ol models.RadiusOnline) error {
	ol.ID = common.UUIDint64()
	err := app.GDB().Create(&ol).Error
	if err != nil {
		return err
	}
	return nil
}

func (s *RadiusService) AddRadiusAccounting(ol models.RadiusOnline, start bool) error {
	accounting := models.RadiusAccounting{
		ID:                common.UUIDint64(),
		Username:          ol.Username,
		AcctSessionId:     ol.AcctSessionId,
		NasId:             ol.NasId,
		NasAddr:           ol.NasAddr,
		NasPaddr:          ol.NasPaddr,
		SessionTimeout:    ol.SessionTimeout,
		FramedIpaddr:      ol.FramedIpaddr,
		FramedNetmask:     ol.FramedNetmask,
		MacAddr:           ol.MacAddr,
		NasPort:           ol.NasPort,
		NasClass:          ol.NasClass,
		NasPortId:         ol.NasPortId,
		NasPortType:       ol.NasPortType,
		ServiceType:       ol.ServiceType,
		AcctSessionTime:   ol.AcctSessionTime,
		AcctInputTotal:    ol.AcctInputTotal,
		AcctOutputTotal:   ol.AcctOutputTotal,
		AcctInputPackets:  ol.AcctInputPackets,
		AcctOutputPackets: ol.AcctOutputPackets,
		LastUpdate:        time.Now(),
		AcctStartTime:     ol.AcctStartTime,
		AcctStopTime:      time.Time{},
	}

	if !start {
		accounting.AcctStopTime = time.Now()
	}
	return app.GDB().Create(&accounting).Error
}

func (s *RadiusService) GetRadiusOnlineCount(username string) int {
	var count int64
	app.GDB().Model(&models.RadiusOnline{}).
		Where("username = ?", username).
		Count(&count)
	return int(count)
}

func (s *RadiusService) ExistRadiusOnline(sessionId string) bool {
	var count int64
	app.GDB().Model(&models.RadiusOnline{}).
		Where("acct_session_id = ?", sessionId).
		Count(&count)
	return count > 0
}

func (s *RadiusService) UpdateRadiusOnlineData(data models.RadiusOnline) error {
	param := map[string]interface{}{
		"acct_input_total":    data.AcctInputTotal,
		"acct_output_total":   data.AcctOutputTotal,
		"acct_input_packets":  data.AcctInputPackets,
		"acct_output_packets": data.AcctOutputPackets,
		"acct_session_time":   data.AcctSessionTime,
		"last_update":         time.Now(),
	}
	return app.GDB().Model(&models.RadiusOnline{}).
		Where("acct_session_id= ?", data.AcctSessionId).
		Updates(&param).Error
}

func (s *RadiusService) EndRadiusAccounting(online models.RadiusOnline) error {
	param := map[string]interface{}{
		"acct_stop_time":      time.Now(),
		"acct_input_total":    online.AcctInputTotal,
		"acct_output_total":   online.AcctOutputTotal,
		"acct_input_packets":  online.AcctInputPackets,
		"acct_output_packets": online.AcctOutputPackets,
		"acct_session_time":   online.AcctSessionTime,
	}
	return app.GDB().Model(&models.RadiusOnline{}).
		Where("acct_session_id= ?", online.AcctSessionId).
		Updates(&param).Error
}

func (s *RadiusService) RemoveRadiusOnline(sessionId string) error {
	return app.GDB().
		Where("acct_session_id = ?", sessionId).
		Delete(&models.RadiusOnline{}).Error
}

func (s *RadiusService) BatchClearRadiusOnline(ids string) error {
	return app.GDB().Where("id in (?)", strings.Split(ids, ",")).Delete(&models.RadiusOnline{}).Error
}

func (s *RadiusService) BatchClearRadiusOnlineByNas(nasip, nasid string) {
	_ = app.GDB().Where("nas_addr = ?", nasip).Delete(&models.RadiusOnline{})
	_ = app.GDB().Where("nas_id = ?", nasid).Delete(&models.RadiusOnline{})
}

func (s *RadiusService) Release() {
	s.TaskPool.Running()
	_ = s.TaskPool.ReleaseTimeout(time.Second * 5)
}

var secretError = errors.New("secret error")

func (s *RadiusService) CheckRequestSecret(r *radius.Packet, secret []byte) {
	request, err := r.MarshalBinary()
	if err != nil {
		panic(err)
	}

	if len(secret) == 0 {
		panic(secretError)
	}

	hash := md5.New()
	hash.Write(request[:4])
	var nul [16]byte
	hash.Write(nul[:])
	hash.Write(request[20:])
	hash.Write(secret)
	var sum [md5.Size]byte
	if !bytes.Equal(hash.Sum(sum[:0]), request[4:20]) {
		panic(secretError)
	}
}
