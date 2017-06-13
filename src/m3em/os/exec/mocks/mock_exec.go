// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

// Automatically generated by MockGen. DO NOT EDIT!
// Source: github.com/m3db/m3em/os/exec/types.go

package exec

import (
	gomock "github.com/golang/mock/gomock"
)

// Mock of ProcessListener interface
type MockProcessListener struct {
	ctrl     *gomock.Controller
	recorder *_MockProcessListenerRecorder
}

// Recorder for MockProcessListener (not exported)
type _MockProcessListenerRecorder struct {
	mock *MockProcessListener
}

func NewMockProcessListener(ctrl *gomock.Controller) *MockProcessListener {
	mock := &MockProcessListener{ctrl: ctrl}
	mock.recorder = &_MockProcessListenerRecorder{mock}
	return mock
}

func (_m *MockProcessListener) EXPECT() *_MockProcessListenerRecorder {
	return _m.recorder
}

func (_m *MockProcessListener) OnComplete() {
	_m.ctrl.Call(_m, "OnComplete")
}

func (_mr *_MockProcessListenerRecorder) OnComplete() *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "OnComplete")
}

func (_m *MockProcessListener) OnError(_param0 error) {
	_m.ctrl.Call(_m, "OnError", _param0)
}

func (_mr *_MockProcessListenerRecorder) OnError(arg0 interface{}) *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "OnError", arg0)
}

// Mock of ProcessMonitor interface
type MockProcessMonitor struct {
	ctrl     *gomock.Controller
	recorder *_MockProcessMonitorRecorder
}

// Recorder for MockProcessMonitor (not exported)
type _MockProcessMonitorRecorder struct {
	mock *MockProcessMonitor
}

func NewMockProcessMonitor(ctrl *gomock.Controller) *MockProcessMonitor {
	mock := &MockProcessMonitor{ctrl: ctrl}
	mock.recorder = &_MockProcessMonitorRecorder{mock}
	return mock
}

func (_m *MockProcessMonitor) EXPECT() *_MockProcessMonitorRecorder {
	return _m.recorder
}

func (_m *MockProcessMonitor) Start() error {
	ret := _m.ctrl.Call(_m, "Start")
	ret0, _ := ret[0].(error)
	return ret0
}

func (_mr *_MockProcessMonitorRecorder) Start() *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "Start")
}

func (_m *MockProcessMonitor) Stop() error {
	ret := _m.ctrl.Call(_m, "Stop")
	ret0, _ := ret[0].(error)
	return ret0
}

func (_mr *_MockProcessMonitorRecorder) Stop() *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "Stop")
}

func (_m *MockProcessMonitor) Running() bool {
	ret := _m.ctrl.Call(_m, "Running")
	ret0, _ := ret[0].(bool)
	return ret0
}

func (_mr *_MockProcessMonitorRecorder) Running() *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "Running")
}

func (_m *MockProcessMonitor) Err() error {
	ret := _m.ctrl.Call(_m, "Err")
	ret0, _ := ret[0].(error)
	return ret0
}

func (_mr *_MockProcessMonitorRecorder) Err() *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "Err")
}

func (_m *MockProcessMonitor) Close() error {
	ret := _m.ctrl.Call(_m, "Close")
	ret0, _ := ret[0].(error)
	return ret0
}

func (_mr *_MockProcessMonitorRecorder) Close() *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "Close")
}

func (_m *MockProcessMonitor) StdoutPath() string {
	ret := _m.ctrl.Call(_m, "StdoutPath")
	ret0, _ := ret[0].(string)
	return ret0
}

func (_mr *_MockProcessMonitorRecorder) StdoutPath() *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "StdoutPath")
}

func (_m *MockProcessMonitor) StderrPath() string {
	ret := _m.ctrl.Call(_m, "StderrPath")
	ret0, _ := ret[0].(string)
	return ret0
}

func (_mr *_MockProcessMonitorRecorder) StderrPath() *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "StderrPath")
}