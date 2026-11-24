// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package fronius

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/simonvetter/modbus"
)

// SunSpec storage model 124 point offsets (relative to the first point,
// i.e. after the model ID and length registers).
const (
	model124         = 124
	offWChaMax       = 0
	offStorCtlMod    = 3
	offOutWRte       = 10
	offInWRte        = 11
	offRvrtTms       = 13
	offWChaMaxSF     = 16
	offInOutWRteSF   = 23
	storCtlCharge    = 1 // bit 0: limit charging by InWRte
	storCtlDischarge = 2 // bit 1: limit discharging by OutWRte
	sunspecBaseAddr  = 40000
)

// registers abstracts the Modbus client for tests.
type registers interface {
	read(addr, qty uint16) ([]uint16, error)
	write(addr uint16, values []uint16) error
}

type sunspec struct {
	mu   sync.Mutex
	regs registers
	base uint16 // address of first point of model 124 (0 = not discovered)
}

func newSunspec(url string, unit uint8) *sunspec {
	return &sunspec{regs: &modbusRegs{url: url, unit: unit}}
}

type modbusRegs struct {
	url    string
	unit   uint8
	client *modbus.ModbusClient
}

func (m *modbusRegs) connect() error {
	if m.client != nil {
		return nil
	}
	c, err := modbus.NewClient(&modbus.ClientConfiguration{URL: m.url, Timeout: 3 * time.Second})
	if err != nil {
		return err
	}
	if err := c.Open(); err != nil {
		return err
	}
	if err := c.SetUnitId(m.unit); err != nil {
		return err
	}
	m.client = c
	return nil
}

func (m *modbusRegs) read(addr, qty uint16) ([]uint16, error) {
	if err := m.connect(); err != nil {
		return nil, err
	}
	v, err := m.client.ReadRegisters(addr, qty, modbus.HOLDING_REGISTER)
	if err != nil {
		m.client.Close()
		m.client = nil
	}
	return v, err
}

func (m *modbusRegs) write(addr uint16, values []uint16) error {
	if err := m.connect(); err != nil {
		return err
	}
	err := m.client.WriteRegisters(addr, values)
	if err != nil {
		m.client.Close()
		m.client = nil
	}
	return err
}

// discover walks the SunSpec model chain to find model 124.
func (s *sunspec) discover() error {
	if s.base != 0 {
		return nil
	}
	hdr, err := s.regs.read(sunspecBaseAddr, 2)
	if err != nil {
		return fmt.Errorf("sunspec: %w", err)
	}
	if hdr[0] != 0x5375 || hdr[1] != 0x6e53 { // "SunS"
		return errors.New("sunspec: marker 'SunS' not found at 40000")
	}
	addr := uint16(sunspecBaseAddr + 2)
	for i := 0; i < 50; i++ {
		h, err := s.regs.read(addr, 2)
		if err != nil {
			return fmt.Errorf("sunspec: %w", err)
		}
		id, length := h[0], h[1]
		if id == 0xFFFF {
			break
		}
		if id == model124 {
			s.base = addr + 2
			return nil
		}
		addr += 2 + length
	}
	return errors.New("sunspec: storage model 124 not found (battery control not available)")
}

func scale(raw uint16) float64 { return math.Pow10(int(int16(raw))) }

// setChargeLimit limits charging to w watts with automatic revert.
func (s *sunspec) setChargeLimit(w float64, revert time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.discover(); err != nil {
		return err
	}
	pts, err := s.regs.read(s.base, offInOutWRteSF+1)
	if err != nil {
		return err
	}
	wChaMax := float64(pts[offWChaMax]) * scale(pts[offWChaMaxSF])
	if wChaMax <= 0 {
		return errors.New("sunspec: WChaMax is zero")
	}
	pct := math.Max(0, math.Min(100, w/wChaMax*100))
	raw := int16(math.Round(pct / scale(pts[offInOutWRteSF])))
	// InWRte first, then revert timeout, then enable the charge limit
	if err := s.regs.write(s.base+offInWRte, []uint16{uint16(raw)}); err != nil {
		return err
	}
	if err := s.regs.write(s.base+offRvrtTms, []uint16{uint16(revert.Seconds())}); err != nil {
		return err
	}
	mode := pts[offStorCtlMod] | storCtlCharge
	return s.regs.write(s.base+offStorCtlMod, []uint16{mode})
}

// reset disables all storage control (autonomous inverter behaviour).
func (s *sunspec) reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.discover(); err != nil {
		return err
	}
	return s.regs.write(s.base+offStorCtlMod, []uint16{0})
}

// setLimits sets charge and discharge limits in one go (negative = no
// limit) with automatic revert; without any limit the inverter returns to
// autonomous operation.
func (s *sunspec) setLimits(chargeW, dischargeW float64, revert time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.discover(); err != nil {
		return err
	}
	if chargeW < 0 && dischargeW < 0 {
		return s.regs.write(s.base+offStorCtlMod, []uint16{0})
	}
	pts, err := s.regs.read(s.base, offInOutWRteSF+1)
	if err != nil {
		return err
	}
	wChaMax := float64(pts[offWChaMax]) * scale(pts[offWChaMaxSF])
	if wChaMax <= 0 {
		return errors.New("sunspec: WChaMax is zero")
	}
	rate := func(w float64) uint16 {
		pct := math.Max(0, math.Min(100, w/wChaMax*100))
		return uint16(int16(math.Round(pct / scale(pts[offInOutWRteSF]))))
	}
	var mode uint16
	if chargeW >= 0 {
		if err := s.regs.write(s.base+offInWRte, []uint16{rate(chargeW)}); err != nil {
			return err
		}
		mode |= storCtlCharge
	}
	if dischargeW >= 0 {
		if err := s.regs.write(s.base+offOutWRte, []uint16{rate(dischargeW)}); err != nil {
			return err
		}
		mode |= storCtlDischarge
	}
	if err := s.regs.write(s.base+offRvrtTms, []uint16{uint16(revert.Seconds())}); err != nil {
		return err
	}
	return s.regs.write(s.base+offStorCtlMod, []uint16{mode})
}
