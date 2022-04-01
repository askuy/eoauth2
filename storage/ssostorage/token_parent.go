package ssostorage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ego-component/eoauth2/server/model"
	"github.com/go-redis/redis/v8"
	"github.com/gotomicro/ego-component/eredis"
	"github.com/spf13/cast"
)

/**
{
	"ctime": 1648785386,
	"uids": [
		1
	],
	"clients": {
		"M7foz8OUQIGqkGklFfNqhw": {
		"token": "M7foz8OUQIGqkGklFfNqhw",
		"authAt": 1648785386,
		"expiresIn": 86400
		}
	},
	"users": {
		"1": {
			"ctime": 1648785385,
			"ua": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/100.0.4896.60 Safari/537.36",
			"clientIp": "::1",
			"platform": "web"
		}
	},
	"expireTimeList": [
		{
			"token": "_c:M7foz8OUQIGqkGklFfNqhw",
			"expireTime": 1648871786
		}
	]
}
*/

type parentToken struct {
	config                *config
	redis                 *eredis.Component
	hashKeyCtime          string
	hashKeyUids           string
	hashKeyExpireTimeList string
	hashKeyUserInfo       string
	hashKeyClient         string
}

func newParentToken(config *config, redis *eredis.Component) *parentToken {
	return &parentToken{
		config:                config,
		redis:                 redis,
		hashKeyCtime:          "_ct",    // create time
		hashKeyUids:           "_u",     // uid
		hashKeyExpireTimeList: "_etl",   // expire time List
		hashKeyClient:         "_c:%s",  // ClientInfo
		hashKeyUserInfo:       "_ui:%d", // UserInfo

	}
}

func (p *parentToken) getKey(pToken string) string {
	return fmt.Sprintf(p.config.parentTokenMapSubTokenKey, pToken)
}

func (p *parentToken) getClientField(subToken string) string {
	return fmt.Sprintf(p.hashKeyClient, subToken)
}

func (p *parentToken) create(ctx context.Context, ssoData model.ParentToken) error {
	// 如果没有开启多账号，那么就是单账号，直接set
	if !p.config.enableMultipleAccounts {
		uids := UidsStore{ssoData.Uid}
		uids.Marshal()
		err := p.redis.HMSet(ctx, p.getKey(ssoData.Token.Token), map[string]interface{}{
			p.hashKeyCtime: time.Now().Unix(),
			p.hashKeyUids:  uids.Marshal(),
			fmt.Sprintf(p.hashKeyUserInfo, ssoData.Uid): ssoData.StoreData.Marshal(),
		}, time.Duration(ssoData.Token.ExpiresIn)*time.Second)
		if err != nil {
			return fmt.Errorf("parentToken.create failed, err:%w", err)
		}
		return nil
	}

	uids, err := p.getUids(ctx, ssoData.Token.Token)
	// 系统错误
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("parentToken.create get key empty, err: %w", err)
	}

	// 如果不存在，那么直接set，创建
	if errors.Is(err, redis.Nil) {
		uids = UidsStore{ssoData.Uid}
		uids.Marshal()
		err = p.redis.HMSet(ctx, p.getKey(ssoData.Token.Token), map[string]interface{}{
			p.hashKeyCtime: time.Now().Unix(),
			p.hashKeyUids:  uids.Marshal(),
			fmt.Sprintf(p.hashKeyUserInfo, ssoData.Uid): ssoData.StoreData.Marshal(),
		}, time.Duration(ssoData.Token.ExpiresIn)*time.Second)
		if err != nil {
			return fmt.Errorf("parentToken.create failed, err:%w", err)
		}
		return nil
	}
	// 如果存在，那么需要取出之前数据，重新写入新的uid信息
	uids = append(uids, ssoData.Uid)
	err = p.redis.HMSet(ctx, p.getKey(ssoData.Token.Token), map[string]interface{}{
		p.hashKeyUids: uids.Marshal(),
		fmt.Sprintf(p.hashKeyUserInfo, ssoData.Uid): ssoData.StoreData.Marshal(),
	}, time.Duration(ssoData.Token.ExpiresIn)*time.Second)
	if err != nil {
		return fmt.Errorf("parentToken.create failed, err:%w", err)
	}
	return nil
}

//func (p *parentToken) renew(ctx context.Context, pToken model.Token) error {
//	err := p.redis.Client().Expire(ctx, p.getKey(pToken.Token), time.Duration(pToken.ExpiresIn)*time.Second).Err()
//	if err != nil {
//		return fmt.Errorf("parentToken.renew failed, err:%w", err)
//	}
//
//	return nil
//}

func (p *parentToken) delete(ctx context.Context, pToken string) error {
	_, err := p.redis.Del(ctx, p.getKey(pToken))
	if err != nil {
		return fmt.Errorf("token.removeParentToken: remove token failed, err:%w", err)
	}
	return nil
}

func (p *parentToken) setToken(ctx context.Context, pToken string, clientId string, token model.Token) error {
	expireTimeList, err := p.getExpireTimeList(ctx, pToken)
	if err != nil {
		return err
	}
	// 因为authorize阶段创建了parent token，所以如果不存在parent token key是有问题的，需要报错
	_, err = p.redis.HGet(ctx, p.getKey(pToken), p.hashKeyCtime)
	if err != nil {
		return fmt.Errorf("parentToken.createToken get key empty, err: %w", err)
	}

	nowTime := time.Now().Unix()
	newExpireTimeList := make(UserTokenExpires, 0)
	// 新数据添加到队列前面，这样方便后续清除数据，或者对数据做一些限制
	newExpireTimeList = append(newExpireTimeList, UserTokenExpire{
		Token:      p.getClientField(token.Token),
		ExpireTime: nowTime + token.ExpiresIn,
	})

	// 删除过期的数据
	hdelFields := make([]string, 0)
	for _, value := range expireTimeList {
		// 过期时间小于当前时间，那么需要删除
		if value.ExpireTime <= nowTime {
			hdelFields = append(hdelFields, value.Token)
			continue
		}
		newExpireTimeList = append(newExpireTimeList, value)
	}
	if len(hdelFields) > 0 {
		err = p.redis.HDel(ctx, p.getKey(pToken), hdelFields...)
		if err != nil {
			return fmt.Errorf("uidMapParentToken setToken HDel expire data failed, error: %w", err)
		}
	}

	err = p.redis.HSet(ctx, p.getKey(pToken), p.hashKeyExpireTimeList, newExpireTimeList.Marshal())
	if err != nil {
		return fmt.Errorf("uidMapParentToken setToken HSet expire time failed, error: %w", err)
	}

	tokenJsonInfo, err := token.Marshal()
	if err != nil {
		return fmt.Errorf("parentToken.createToken json marshal failed, err: %w", err)
	}

	err = p.redis.HSet(ctx, p.getKey(pToken), p.getClientField(token.Token), tokenJsonInfo)
	if err != nil {
		return fmt.Errorf("parentToken.createToken hset failed, err:%w", err)
	}
	return nil
}

func (p *parentToken) getUids(ctx context.Context, pToken string) (uids UidsStore, err error) {
	uidBytes, err := p.redis.Client().HGet(ctx, p.getKey(pToken), p.hashKeyUids).Bytes()
	// 系统错误
	if err != nil {
		err = fmt.Errorf("getUids failed, err: %w", err)
		return
	}

	err = uids.Unmarshal(uidBytes)
	if err != nil {
		err = fmt.Errorf("parentToken.create unmarshal err: %w", err)
		return
	}
	return
}

// 获取过期时间，最新的在最前面。
func (p *parentToken) getExpireTimeList(ctx context.Context, pToken string) (uidTokenInfo UserTokenExpires, err error) {
	// 根据父节点token，获取用户信息
	infoBytes, err := p.redis.Client().HGet(ctx, p.getKey(pToken), p.hashKeyExpireTimeList).Bytes()
	if err != nil && !errors.Is(err, redis.Nil) {
		err = fmt.Errorf("parentToken getExpireTimeList failed, err: %w", err)
		return
	}
	if errors.Is(err, redis.Nil) {
		err = nil
		return
	}

	pUserInfo := &uidTokenInfo
	err = pUserInfo.Unmarshal(infoBytes)
	if err != nil {
		err = fmt.Errorf("parentToken getExpireTimeList json unmarshal error, %w", err)
		return
	}
	return
}

// todo client id记录token会有问题，多账号是无法使用，还需要优化
//func (p *parentToken) getToken(ctx context.Context, pToken string, clientId string) (tokenInfo model.Token, err error) {
//	tokenValue, err := p.redis.HGet(ctx, p.getKey(pToken), p.getClientField(clientId))
//	if err != nil {
//		err = fmt.Errorf("tokgen get redis hmget string error, %w", err)
//		return
//	}
//	pTokenInfo := &tokenInfo
//	err = pTokenInfo.Unmarshal([]byte(tokenValue))
//	if err != nil {
//		err = fmt.Errorf("redis token info json unmarshal errorr, err: %w", err)
//		return
//	}
//	return
//}

func (p *parentToken) getAll(ctx context.Context, pToken string) (output *ParentTokenStore, err error) {
	allInfo, err := p.redis.Client().HGetAll(ctx, p.getKey(pToken)).Result()
	if err != nil {
		err = fmt.Errorf("tokgen get redis hmget string error, %w", err)
		return
	}
	output = &ParentTokenStore{
		Ctime:          0,
		Uids:           &UidsStore{},
		Clients:        make(map[string]*model.Token),
		Users:          make(map[int64]*model.ParentTokenData),
		ExpireTimeList: &UserTokenExpires{},
	}
	for key, value := range allInfo {
		output.processData(key, value)
	}
	ttl, err := p.redis.Client().TTL(ctx, p.getKey(pToken)).Result()
	if err != nil {
		err = fmt.Errorf("parentToken getAll failed,err: %w", err)
		return
	}
	output.TTL = ttl.Milliseconds() / 1000
	return
}

// ParentTokenStore 存储的所有信息
type ParentTokenStore struct {
	Ctime          int64                            `json:"ctime"`
	Uids           *UidsStore                       `json:"uids"`
	Clients        map[string]*model.Token          `json:"clients"`
	Users          map[int64]*model.ParentTokenData `json:"users"`
	ExpireTimeList *UserTokenExpires                `json:"expireTimeList"`
	TTL            int64                            `json:"ttl"`
}

func (p *ParentTokenStore) processData(key string, value string) {
	switch true {
	case key == "_ct":
		p.Ctime = cast.ToInt64(value)
	case key == "_u":
		p.Uids.Unmarshal([]byte(value))
	case key == "_etl":
		p.ExpireTimeList.Unmarshal([]byte(value))
	case strings.HasPrefix(key, "_ui:"):
		arr := strings.Split(key, ":")
		if len(arr) != 2 {
			return
		}
		uid := cast.ToInt64(arr[1])
		if uid == 0 {
			return
		}
		data := &model.ParentTokenData{}
		err := data.Unmarshal([]byte(value))
		if err != nil {
			return
		}
		p.Users[uid] = data
	case strings.HasPrefix(key, "_c:"):
		arr := strings.Split(key, ":")
		if len(arr) != 2 {
			return
		}
		data := &model.Token{}
		err := data.Unmarshal([]byte(value))
		if err != nil {
			return
		}
		p.Clients[arr[1]] = data
	}
}
