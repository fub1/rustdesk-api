package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"io"
	"net"
	"strconv"
	"time"
)

type ServerCmdService struct{}

// List
func (is *ServerCmdService) List(page, pageSize uint) (res *model.ServerCmdList) {
	res = &model.ServerCmdList{}
	res.Page = int64(page)
	res.PageSize = int64(pageSize)
	tx := DB.Model(&model.ServerCmd{})
	tx.Count(&res.Total)
	tx.Scopes(Paginate(page, pageSize))
	tx.Find(&res.ServerCmds)
	return
}

// Info
func (is *ServerCmdService) Info(id uint) *model.ServerCmd {
	u := &model.ServerCmd{}
	DB.Where("id = ?", id).First(u)
	return u
}

// Delete
func (is *ServerCmdService) Delete(u *model.ServerCmd) error {
	return DB.Delete(u).Error
}

// Create
func (is *ServerCmdService) Create(u *model.ServerCmd) error {
	res := DB.Create(u).Error
	return res
}

// SendCmd 发送命令
func (is *ServerCmdService) SendCmd(port int, cmd string, arg string) (string, error) {
	return is.SendCmdContext(context.Background(), port, cmd, arg)
}

func (is *ServerCmdService) SendCmdContext(ctx context.Context, port int, cmd, arg string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := cmd + " " + arg
	res, connected, err := sendSocketCmd(ctx, "v6", port, command)
	if err == nil || connected || ctx.Err() != nil {
		return res, err
	}
	res, _, err = sendSocketCmd(ctx, "v4", port, command)
	return res, err
}

// SendSocketCmd
func (is *ServerCmdService) SendSocketCmd(ty string, port int, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, _, err := sendSocketCmd(ctx, ty, port, cmd)
	return res, err
}

// Once connected, a write or read failure cannot safely be retried on another address.
func sendSocketCmd(ctx context.Context, ty string, port int, cmd string) (string, bool, error) {
	addr := "::1"
	tcp := "tcp6"
	if ty == "v4" {
		tcp = "tcp"
		addr = "127.0.0.1"
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, tcp, net.JoinHostPort(addr, strconv.Itoa(port)))
	if err != nil {
		return "", false, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return "", true, err
		}
	}
	n, err := conn.Write([]byte(cmd))
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return "", true, err
	}
	if n != len(cmd) {
		return "", true, io.ErrShortWrite
	}
	buf := make([]byte, 1024)
	n, err = conn.Read(buf)
	if ctx.Err() != nil {
		return "", true, ctx.Err()
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", true, fmt.Errorf("read server command response: %w", err)
	}
	return string(buf[:n]), true, nil
}

func (is *ServerCmdService) Update(f *model.ServerCmd) error {
	return DB.Model(f).Updates(f).Error
}
