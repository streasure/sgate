package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	protocol "github.com/streasure/protocol/gateway"
	logicproto "github.com/streasure/protocol/logic"
	"github.com/streasure/sgate/bench/logutil"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

type loginResponse struct {
	Code int `json:"code"`
	Data struct {
		AccountID  string `json:"accountId"`
		LoginToken string `json:"loginToken"`
	} `json:"data"`
}

func getLoginCredentials(addr, openID string) (string, string, error) {
	body, err := json.Marshal(map[string]string{"openId": openID})
	if err != nil {
		return "", "", err
	}
	resp, err := http.Post("http://"+addr+"/api/v1/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	var result loginResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return "", "", err
	}
	if result.Code != 0 || result.Data.AccountID == "" || result.Data.LoginToken == "" {
		return "", "", errors.New("loginserver returned invalid credentials response")
	}
	return result.Data.AccountID, result.Data.LoginToken, nil
}

func writeFrame(conn net.Conn, frame *protocol.MessageFrame) error {
	body, err := proto.Marshal(frame)
	if err != nil {
		return err
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(body)))
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err = conn.Write(body)
	return err
}

func readFrame(conn net.Conn) (*protocol.MessageFrame, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	body := make([]byte, binary.BigEndian.Uint32(header))
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, err
	}
	frame := new(protocol.MessageFrame)
	if err := proto.Unmarshal(body, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

func main() {
	loginserver := flag.String("loginserver", "127.0.0.1:10001", "loginserver HTTP address")
	sgate := flag.String("sgate", "127.0.0.1:48080", "sgate TCP address")
	serverID := flag.String("server-id", "logic-logincheck", "logic server ID")
	logConfig := flag.String("config", "../logincheck/config/log.yaml", "log configuration")
	flag.Parse()
	defer logutil.Init(*logConfig)()

	accountID, loginToken, err := getLoginCredentials(*loginserver, "logincheck-openid")
	if err != nil {
		panic(err)
	}
	conn, err := net.DialTimeout("tcp", *sgate, 5*time.Second)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	gateBody, _ := proto.Marshal(&protocol.LoginGateReq{ServerId: *serverID, UserId: accountID, LoginKey: loginToken})
	if err := writeFrame(conn, &protocol.MessageFrame{Cmd: routes.CmdLoginGate, SeqId: 1, Body: gateBody}); err != nil {
		panic(err)
	}
	gateAck, err := readFrame(conn)
	if err != nil {
		panic(err)
	}
	if gateAck.Cmd != routes.CmdLoginGateAck {
		panic("unexpected LoginGateAck command")
	}
	ack := new(protocol.LoginGateAck)
	if err := proto.Unmarshal(gateAck.Body, ack); err != nil {
		panic(err)
	}
	if ack.Code != 0 {
		panic(fmt.Errorf("LoginGateReq rejected code=%d message=%s", ack.Code, ack.Message))
	}

	loginBody, _ := proto.Marshal(&logicproto.LoginReq{UserId: accountID, LoginKey: loginToken, Channel: 1})
	if err := writeFrame(conn, &protocol.MessageFrame{Cmd: routes.CmdLogicLoginReq, SeqId: 2, Body: loginBody}); err != nil {
		panic(err)
	}
	loginAckFrame, err := readFrame(conn)
	if err != nil {
		panic(err)
	}
	if loginAckFrame.Cmd != routes.CmdLogicLoginAck {
		panic("unexpected LoginAck command")
	}
	loginAck := new(logicproto.LoginAck)
	if err := proto.Unmarshal(loginAckFrame.Body, loginAck); err != nil {
		panic(err)
	}
	tlog.Info(context.TODO(), "login flow succeeded accountId=%s userKey=%s version=%s", accountID, loginAck.UserKey, loginAck.Version)
}
