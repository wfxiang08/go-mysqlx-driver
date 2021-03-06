// Go driver for MySQL X Protocol
// Based heavily on Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2012 The Go-MySQL-Driver Authors. All rights reserved.
// Copyright 2016 Simon J Mudd.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import (
	"database/sql/driver"
	"fmt"
	"log"

	"github.com/golang/protobuf/proto"

	"github.com/sjmudd/go-mysqlx-driver/Mysqlx"
	"github.com/sjmudd/go-mysqlx-driver/Mysqlx_Connection"
	"github.com/sjmudd/go-mysqlx-driver/Mysqlx_Datatypes"
	"github.com/sjmudd/go-mysqlx-driver/Mysqlx_Notice"
	"github.com/sjmudd/go-mysqlx-driver/Mysqlx_Session"
	"github.com/sjmudd/go-mysqlx-driver/Mysqlx_Sql"

	"github.com/sjmudd/go-mysqlx-driver/debug"
)

var (
	// this seems not to be defined in the protobuf specification
	mysqlxNoticeTypeName = map[uint32]string{
		1: "Warning",
		2: "SessionVariableChanged",
		3: "SessionStateChanged",
	}
)

// netProtobuf holds the protobuf message type and the network bytes from a protobuf message
// - see docs at ....
type netProtobuf struct {
	msgType int
	payload []byte
}

// Read a raw netProtobuf packet from the network and return a pointer to the structure
func (mc *mysqlXConn) readMsg() (*netProtobuf, error) {
	// Read packet header
	data, err := mc.buf.readNext(4)
	if err != nil {
		errLog.Print(err)
		mc.Close()
		return nil, driver.ErrBadConn
	}

	// Packet Length [32 bit]
	pktLen := int(uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16 | uint32(data[3])<<24)

	if pktLen < minPacketSize {
		errLog.Print(ErrMalformPkt)
		mc.Close()
		return nil, driver.ErrBadConn
	}

	// Read body which is 1-byte msg type and 0+ bytes payload
	data, err = mc.buf.readNext(pktLen)
	if err != nil {
		errLog.Print(err)
		mc.Close()
		return nil, driver.ErrBadConn
	}

	pb := &netProtobuf{
		msgType: int(data[0]),
	}
	if len(data) > 1 {
		pb.payload = data[1:]
	}

	debug.MsgProtobuf("S -> C: len: %d, type: %d [%s], payload: %+v",
		5+len(pb.payload),
		pb.msgType,
		Mysqlx.ServerMessages_Type(pb.msgType).String(),
		pb.payload)
	return pb, nil
}

// Write packet buffer 'data'
func (mc *mysqlXConn) writeProtobufPacket(pb *netProtobuf) error {
	if pb == nil || pb.msgType > 255 {
		mc.Close()
		return ErrMalformPkt
	}
	debug.MsgProtobuf("C -> S: len: %d, type: %d [%s], payload: %+v",
		5+len(pb.payload),
		pb.msgType, Mysqlx.ClientMessages_Type(pb.msgType).String(),
		pb.payload)

	pktLen := len(pb.payload) + 1

	if pktLen > mc.maxPacketAllowed {
		return ErrPktTooLarge
	}

	// setup initial header
	data := make([]byte, 5)

	var size int
	data[0] = byte(pktLen)
	data[1] = byte(pktLen >> 8)
	data[2] = byte(pktLen >> 16)
	data[3] = byte(pktLen >> 24)
	data[4] = byte(pb.msgType)
	size = pktLen

	// Write header
	n, err := mc.netConn.Write(data)
	if err != nil || n != 5 {
		return fmt.Errorf("Error writing protobuf header to socket, wrote %d of 5 bytes: %v", n, err)
	}

	// Write payload
	n, err = mc.netConn.Write(pb.payload)
	if err != nil && n != size {
		return fmt.Errorf("Error writing protobuf body to socket, wrote %d of %d bytes: %v", n, size, err)
	}
	return nil
}

/******************************************************************************
*                           Initialisation Process                            *
******************************************************************************/

// helper function - check this is a a scalar type
func isScalar(value *Mysqlx_Datatypes.Any) bool {
	return value != nil && *value.Type == Mysqlx_Datatypes.Any_SCALAR
}

// helper function - check this is a scalar VString
func isScalarString(value *Mysqlx_Datatypes.Any) bool {
	return value != nil && *value.Type == Mysqlx_Datatypes.Any_SCALAR && *value.Scalar.Type == Mysqlx_Datatypes.Scalar_V_STRING
}

// helper function - check this is a a scalar VBool
func isScalarBool(value *Mysqlx_Datatypes.Any) bool {
	return value != nil && *value.Type == Mysqlx_Datatypes.Any_SCALAR && *value.Scalar.Type == Mysqlx_Datatypes.Scalar_V_BOOL
}

// helper function - return the scalar VString as a string
func scalarString(value *Mysqlx_Datatypes.Any) string {
	if !isScalarString(value) {
		return ""
	}
	return string(value.Scalar.VString.Value)
}

// helper function - return the scalar VBool as a bool
func scalarBool(value *Mysqlx_Datatypes.Any) bool {
	if !isScalarBool(value) {
		return false
	}
	return bool(*value.Scalar.VBool)
}

// helper function - check this is an array of VString
func isArrayString(value *Mysqlx_Datatypes.Any) bool {
	if value == nil {
		return false
	}
	if value.GetType() != Mysqlx_Datatypes.Any_ARRAY {
		return false
	}
	for i := range value.GetArray().GetValue() {
		if value.GetArray().GetValue()[i].GetType() != Mysqlx_Datatypes.Any_SCALAR {
			return false
		}
		if value.GetArray().GetValue()[i].GetScalar().GetType() != Mysqlx_Datatypes.Scalar_V_STRING {
			return false
		}
	}

	return true
}

// helper function - return the array of scalar VString as a []string
func arrayString(value *Mysqlx_Datatypes.Any) []string {
	if !isArrayString(value) {
		return nil
	}
	values := []string{}

	for i := range value.GetArray().GetValue() {
		values = append(values, scalarString(value.GetArray().GetValue()[i]))
	}

	return values
}

// Request the getCapabilities
// see: http://.....
func (mc *mysqlXConn) getCapabilities() error {
	// debug.Msg("Enter getCapabilities()")

	if err := mc.writeConnCapabilitiesGet(); err != nil {
		return fmt.Errorf("getCapabilities: %v", err)
	}

	var pb *netProtobuf
	var err error
	done := false
	// wait for the answer CONN_CAPABILITIES, but handle ERROR or NOTICE
	for !done {
		// debug.Msg("getCapabilities() read response to CapabilitiesGet...")
		if pb, err = mc.readMsg(); err != nil {
			return err
		}
		if pb == nil {
			return fmt.Errorf("getCapabilities() pb = nil (not expected to happen ever)")
		}

		// debug.Msg("getCapabilities() have data: type: %s, data: %+v", Mysqlx.ServerMessages_Type(pb.msgType).String(), pb.payload)

		switch Mysqlx.ServerMessages_Type(pb.msgType) {
		case Mysqlx.ServerMessages_ERROR:
			debug.Msg("getCapabilities() got back ERROR msg", errorMsg(pb.payload).Error())
			return fmt.Errorf("getCapabilities returned: %+v", errorMsg(pb.payload))
		case Mysqlx.ServerMessages_CONN_CAPABILITIES:
			// debug.Msg("getCapabilities() CONN_CAPABILITIES msg")
			done = true
		case Mysqlx.ServerMessages_NOTICE: // we don't expect a notice here so just print it.
			debug.Msg("got unexpected NOTICE (see below), ignoring")
			mc.pb = pb                          // hack though maybe should always use mc
			mc.processNotice("getCapabilities") // process the notice message
		default:
			debug.Msg("got unexpected message type (show the type here), ignoring")
		}
	}

	// debug.Msg("getCapabilities() exit loop")

	if pb == nil {
		return fmt.Errorf("BUG: Empty pb")
	}
	if pb.payload == nil {
		return fmt.Errorf("BUG: Empty pb.payload")
	}

	// debug.Msg("getCapabilities() payload has data, %d bytes", len(pb.payload))

	// get the capabilities info
	capabilities := &Mysqlx_Connection.Capabilities{}
	if err := proto.Unmarshal(pb.payload, capabilities); err != nil {
		return fmt.Errorf("unmarshaling error with capabilities: %v", err)
	}

	debug.Msg("found %d capabilities", len(capabilities.GetCapabilities()))
	for i := range capabilities.GetCapabilities() {
		name := capabilities.GetCapabilities()[i].GetName()
		value := capabilities.GetCapabilities()[i].GetValue()
		if isScalar(value) {
			if isScalarString(value) {
				scalar := scalarString(value)
				debug.Msg("- scalar string: name: %q, value: %q", name, scalar)
				mc.capabilities.AddScalarString(name, scalar)
			} else if isScalarBool(value) {
				scalar := scalarBool(value)
				debug.Msg("- scalar bool: name: %q, value: %v", name, scalar)
				mc.capabilities.AddScalarBool(name, scalar)
			} else {
				scalarName := value.Scalar.Type.String()
				debug.Msg("Found scalar: name: %q, value: %+v (type %s) which I can not handle yet", name, value, scalarName)
			}
		} else if isArrayString(value) {
			values := arrayString(value)
			debug.Msg("- array of strings: name: %q, values: %+v", name, values)
			mc.capabilities.AddArrayString(name, values)
		} else {
			valueType := ""
			if isScalar(value) {
				valueType = "scalar"
			} else {
				valueType = "non-scalar"
			}
			scalarName := value.Scalar.Type.String()
			debug.Msg("getCapabilities: capability %q is of a %s type (%s) I can not handle yet (so ignoring)", name, valueType, scalarName)
		}
	}

	return nil
}

// return a new boolean scalar of the given type
func newBoolScalar(value bool) *Mysqlx_Datatypes.Scalar {
	vBool := new(Mysqlx_Datatypes.Scalar_Type)
	*vBool = Mysqlx_Datatypes.Scalar_V_BOOL

	_value := new(bool)
	*_value = value

	// generate the value we want to use
	return &Mysqlx_Datatypes.Scalar{
		Type:  vBool,
		VBool: _value,
	}
}

// return a datatype any of type scalar
func newAnyScalar(anyType Mysqlx_Datatypes.Any_Type, scalar *Mysqlx_Datatypes.Scalar) *Mysqlx_Datatypes.Any {
	someType := new(Mysqlx_Datatypes.Any_Type)
	*someType = anyType

	return &Mysqlx_Datatypes.Any{
		Type:   someType,
		Scalar: scalar,
	}
}

// set a boolean scalar capability (tls probably)
func (mc *mysqlXConn) setScalarBoolCapability(name string, value bool) error {
	debug.Msg("setScalarBoolCapability(%q,%v)", name, value)

	// Wow this is long-winded and harder than I'd expect (even
	// for a trivial setup like this)
	// - definitely need some helper routines above the basic stuff that Go provides.
	// - and good we don't need to free up all this stuff
	//   ourselves afterwards and can leave it to Go's garbage
	//   collector.

	any := newAnyScalar(Mysqlx_Datatypes.Any_SCALAR, newBoolScalar(value))

	// setup capability structure from what we've just created
	capability := &Mysqlx_Connection.Capability{
		Name:  proto.String(name),
		Value: any,
	}

	var capabilitiesArray []*Mysqlx_Connection.Capability
	capabilitiesArray = append(capabilitiesArray, capability)

	capabilities := &Mysqlx_Connection.Capabilities{
		Capabilities: capabilitiesArray,
	}

	// setup CapabilitiesSet structure from what we've just created
	capabilitiesSet := &Mysqlx_Connection.CapabilitiesSet{
		Capabilities: capabilities,
	}

	debug.Msg("CapabilitiesSet msg: <%+v>", capabilitiesSet.String())

	var err error
	pb := new(netProtobuf)
	pb.msgType = int(Mysqlx.ClientMessages_CON_CAPABILITIES_SET)
	if pb.payload, err = proto.Marshal(capabilitiesSet); err != nil {
		return fmt.Errorf("SetScalarBoolCapability(%q,%v) failed to create marshalled message: %v", err)
	}

	debug.Msg("CapabilitySet message: %s", capabilitiesSet.String())

	// Send the message
	if err = mc.writeProtobufPacket(pb); err != nil {
		return fmt.Errorf("SetScalarBoolCapability(%q,%v) failed: %v", name, value, err)
	}

	// wait for the answer (I expect it to be OK / ERROR)
	done := false
	// wait for the answer OK, ERROR or NOTICE
	for !done {
		debug.Msg("setScalarBoolCapability() read response from capabilitiesSet message...")
		if pb, err = mc.readMsg(); err != nil {
			return err
		}
		if pb == nil {
			return fmt.Errorf("setScalarBoolCapability() pb = nil (not expected to happen ever)")
		}

		// debug.Msg("setScalarBoolCapability() have data: type: %s, data: %+v", Mysqlx.ServerMessages_Type(pb.msgType).String(), string(pb.payload))

		switch Mysqlx.ServerMessages_Type(pb.msgType) {
		case Mysqlx.ServerMessages_OK:
			debug.Msg("setScalarBoolCapability() OK msg")
			return nil
		case Mysqlx.ServerMessages_ERROR:
			debug.Msg("setScalarBoolCapability() %s", pb.errorMsg().Error())
			return fmt.Errorf("setScalarBoolCapability failed: %v", mc.processErrorMsg())
		case Mysqlx.ServerMessages_NOTICE:
			// we don't expect a notice here so just print it.
			debug.Msg("got unexpected NOTICE (below), ignoring")
			mc.pb = pb // should use just mc.pb ??
			mc.processNotice("setScalarBoolCapability")
		default:
			debug.Msg("got unexpected message type %d, ignoring", Mysqlx.ServerMessages_Type(pb.msgType))
		}
	}

	// debug.Msg("getCapabilities() exit loop")

	if pb == nil {
		return fmt.Errorf("BUG: Empty pb")
	}
	if pb.payload == nil {
		return fmt.Errorf("BUG: Empty pb.payload")
	}

	// debug.Msg("getCapabilities() payload has data, %d bytes", len(pb.payload))
	return nil
}

// generate an error based on the Mysql.Error message
func errorText(e *Mysqlx.Error) error {
	if e == nil {
		return fmt.Errorf("errorText: ERROR e == nil")
	}
	return fmt.Errorf("%v: %04d [%s] %s", e.Severity, *(e.Code), *(e.SqlState), *(e.Msg))
}

// return an error message type as an error
// - pb MUST BE a protobuf message of type Error
func (pb *netProtobuf) errorMsg() error {
	if pb == nil {
		return fmt.Errorf("errorMsg: ERROR pb == nil")
	}
	if pb.msgType != int(Mysqlx.ServerMessages_ERROR) {
		return fmt.Errorf("errorMsg: ERROR msgType = %d, expecting ERROR (%d)", pb.msgType, Mysqlx.ServerMessages_ERROR)
	}
	e := new(Mysqlx.Error)
	if err := proto.Unmarshal(pb.payload, e); err != nil {
		return fmt.Errorf("unmarshaling error with e: %+v", err)
	}
	return errorText(e)
}

// return an error message type as an error
// - input is a protobuf message of type Error
func errorMsg(data []byte) error {
	e := new(Mysqlx.Error)
	if err := proto.Unmarshal(data, e); err != nil {
		log.Fatal("unmarshaling error with e: ", err)
	}
	return errorText(e)
}

func printAuthenticateOk(data []byte) {
	ok := &Mysqlx_Session.AuthenticateOk{}
	if err := proto.Unmarshal(data, ok); err != nil {
		log.Fatal("unmarshaling error with AuthenticateOk: ", err)
	}

	okAuthData := []byte(ok.GetAuthData())
	debug.Msg("Login successful: Got back authData: %q (%d bytes)", okAuthData, len(okAuthData))
}

func (mc *mysqlXConn) processNotice(where string) error {
	debug.Msg("mysqlXConn.processNotice(%q)", where)
	if mc == nil {
		log.Fatalf("mysqlXConn.processNotice(%q): mc == nil", where)
	}
	if mc.pb == nil {
		log.Fatalf("mysqlXConn.processNotice(%q): mc.pb == nil", where)
	}

	var payload string

	f := new(Mysqlx_Notice.Frame)
	if err := proto.Unmarshal(mc.pb.payload, f); err != nil {
		log.Fatalf("error unmarshaling Notice f: %v", err)
	}

	switch f.GetType() {
	case 1: // warning
		{
			w := new(Mysqlx_Notice.Warning)
			if err := proto.Unmarshal(f.Payload, w); err != nil {
				log.Fatalf("error unmarshaling Warning w: %v", err)
			}
			payload = fmt.Sprintf("Level: %+v, code: %d, msg: %s",
				w.GetLevel().String(),
				w.GetCode(),
				w.GetMsg())
		}
	case 2: // session variable change
		{

			s := new(Mysqlx_Notice.SessionVariableChanged)
			if err := proto.Unmarshal(f.Payload, s); err != nil {
				log.Fatalf("error unmarshaling SessionVariableChanged s: %v", err)
			}
			payload = fmt.Sprintf("SessionVariableChanged: Param: %s, Value: %+v",
				s.GetParam(),
				s.GetValue()) // show value properly
		}
	case 3: // SessionStateChanged
		{
			s := new(Mysqlx_Notice.SessionStateChanged)
			if err := proto.Unmarshal(f.Payload, s); err != nil {
				log.Fatalf("error unmarshaling SessionStateChanged s: %v", err)
			}
			payload = fmt.Sprintf("SessionStateChanged: Param: %s, Value: %+v",
				s.GetParam(),
				s.GetValue()) // show value properly
		}
	default:
		{
			debug.Msg("WARNING: Unable to handle Notice type: %d, ignoring", f.GetType())
			payload = fmt.Sprintf("% x", f.Payload)
		}
	}

	debug.Msg("Received NOTICE: Type: %+v [%s], Scope: %d [%s], %s",
		f.GetType(),
		noticeTypeToName(f.GetType()), // not available by protobuf ?
		f.GetScope(),
		f.GetScope().String(),
		payload)

	mc.pb = nil // reset message (as now processed)

	return nil
}

func (mc *mysqlXConn) writeConnCapabilitiesGet() error {
	pb := new(netProtobuf)
	pb.msgType = int(Mysqlx.ClientMessages_CON_CAPABILITIES_GET)
	// EMPTY PAYLOAD

	return mc.writeProtobufPacket(pb)
}

func (mc *mysqlXConn) writeSessAuthenticateStart(m *Mysqlx_Session.AuthenticateStart) error {
	var err error

	debug.Msg("writeSessAuthenticateStart: %+v", m.String())

	pb := new(netProtobuf)
	pb.msgType = int(Mysqlx.ClientMessages_SESS_AUTHENTICATE_START)
	pb.payload, err = proto.Marshal(m)
	if err != nil {
		return fmt.Errorf("Failed to marshall SesstionAuthenticateStart: %v", err)
	}

	return mc.writeProtobufPacket(pb)
}

func (mc *mysqlXConn) writeSessAuthenticateContinue(m *Mysqlx_Session.AuthenticateContinue) error {
	var err error

	debug.Msg("writeSessAuthenticateContinue: %+v", m.String())

	pb := new(netProtobuf)
	pb.msgType = int(Mysqlx.ClientMessages_SESS_AUTHENTICATE_CONTINUE)
	pb.payload, err = proto.Marshal(m)
	if err != nil {
		return fmt.Errorf("Failed to marshall SessAuthenticateContinue: %v", err)
	}
	return mc.writeProtobufPacket(pb)
}

func readSessAuthenticateContinue(pb *netProtobuf) *Mysqlx_Session.AuthenticateContinue {
	authenticateContinue := &Mysqlx_Session.AuthenticateContinue{}
	if err := proto.Unmarshal(pb.payload, authenticateContinue); err != nil {
		log.Fatal("unmarshaling error with authenticateContinue: ", err)
	}

	debug.Msg("readSessAuthenticateContinue: %+v", authenticateContinue.String())

	return authenticateContinue
}

// AuthenticateMySQL41 uses MYSQL41 authentication method
func (mc *mysqlXConn) AuthenticateMySQL41() error {
	var err error
	debug.Msg("AuthenticateMySQL41(db: %q, user: %q, passwd: <not shown>)", mc.cfg.dbname, mc.cfg.user)

	// ------------------------------------------------------------------------
	// C -> S   SESS_AUTHENTICATE_START
	// ------------------------------------------------------------------------
	authInfo := NewMySQL41(mc.cfg.dbname, mc.cfg.user, mc.cfg.passwd) // copy me into AuthData: ... (and adjust)

	// create the protobuf message (AuthenticateStart)
	msg := &Mysqlx_Session.AuthenticateStart{
		MechName: proto.String("MYSQL41"),
		AuthData: []byte(authInfo.GetInitialAuthData()),
	}
	mc.writeSessAuthenticateStart(msg)
	if err != nil {
		return fmt.Errorf("AuthenticateMySQL41: %v", err)
	}

	// ------------------------------------------------------------------------
	// S -> C   SESS_AUTHENTICATE_CONTINUE
	// ------------------------------------------------------------------------

	// wait for the answer
	pb, err := mc.readMsg()
	if err != nil {
		return err
	}

	if Mysqlx.ServerMessages_Type(pb.msgType) != Mysqlx.ServerMessages_SESS_AUTHENTICATE_CONTINUE {
		return fmt.Errorf("Got unexpected message type back: %s, expecting: %s",
			printableMsgTypeIn(Mysqlx.ServerMessages_Type(pb.msgType)),
			printableMsgTypeIn(Mysqlx.ServerMessages_SESS_AUTHENTICATE_CONTINUE))
	}

	authenticateContinue := readSessAuthenticateContinue(pb)

	authData := []byte(authenticateContinue.GetAuthData())
	//	debug.Msg("- unmarshalled authData, len: %d:%s", len(authData), hex.Dump(authData))
	if len(authData) != 20 {
		return fmt.Errorf("Received %d bytes from server, expecting: 20", len(authData))
	}

	// ------------------------------------------------------------------------
	// C -> S   SESS_AUTHENTICATE_CONTINUE with scrambled password
	// ------------------------------------------------------------------------
	{
		authenticateContinue := &Mysqlx_Session.AuthenticateContinue{}
		response, err := authInfo.GetNextAuthData(authData)
		if err != nil {
			return fmt.Errorf("authInfo.GetNextAuthData() gave an error: %v", err)
		}
		authenticateContinue.AuthData = []byte(response)

		if err := mc.writeSessAuthenticateContinue(authenticateContinue); err != nil {
			return fmt.Errorf("AuthenticateMySQL41: failed writing AuthenticateContinue: %v", err)
		}
	}

	// ------------------------------------------------------------------------
	// S -> C   SESS_AUTHENTICATE_OK / ERROR / NOTICE
	// ------------------------------------------------------------------------
	if err := mc.waitingForAuthenticateOk(); err != nil {
		return fmt.Errorf("Failed to read message response from our SESS_AUTHENTICATE_CONTINUE: %v", err)
	}

	printAuthenticateOk(mc.pb.payload)
	mc.pb = nil // treat the incoming message as processsed

	return nil // supposedly we have done the right thing
}

// waitingForAuthenticateOk is expecting to receive SESS_AUTHENTICATE_OK indicating success.
// We may get an error (of the form: 1045 [HY000] Invalid user or password which needs
// to be passed to the caller so that the connection is closed.
func (mc *mysqlXConn) waitingForAuthenticateOk() error {
	debug.Msg("WaitingForAuthenticateOk: starting")
	var err error
	done := false
	for !done {
		debug.Msg("WaitingForAuthenticateOk: wait for message...")
		mc.pb, err = mc.readMsg()
		if err != nil {
			return fmt.Errorf("Failed to read message response from our SESS_AUTHENTICATE_CONTINUE: %v", err)
		}
		debug.Msg("WaitingForAuthenticateOk: msg read from server")

		switch Mysqlx.ServerMessages_Type(mc.pb.msgType) {
		case Mysqlx.ServerMessages_SESS_AUTHENTICATE_OK:
			{
				done = true /* fall through */
				debug.Msg("WaitingForAuthenticateOk: found expected SESS_AUTHENTICATE_OK")
			}
		case Mysqlx.ServerMessages_ERROR:
			{
				debug.Msg("WaitingForAuthenticateOk: found ERROR, returning to caller")
				return errorMsg(mc.pb.payload)
			}
		case Mysqlx.ServerMessages_NOTICE:
			{
				// Not currently documented (explicitly) but we always get this type of message prior to SESS_AUTHENTICATE_OK
				// debug.Msg("WaitingForAuthenticateOk: found NOTICE, not really expecting")
				if err := mc.processNotice("waitingForAuthenticateOk"); err != nil {
					// just carry on but record the problem. handle properly later
					debug.Msg("waitingForAuthenticateOk: processNotice() failed: %v", err)
				}
			}
		default:
			{
				debug.Msg("Received unexpected message type: %s",
					printableMsgTypeIn(Mysqlx.ServerMessages_Type(mc.pb.msgType)))
				debug.Msg("Expected message type: %s",
					printableMsgTypeIn(Mysqlx.ServerMessages_OK))

				log.Fatalf("mysqlXConn.waitingfor_SESS_AUTHENTICATE_OK: Received unexpected message type: %s, expecting: %s",
					printableMsgTypeIn(Mysqlx.ServerMessages_Type(mc.pb.msgType)),
					printableMsgTypeIn(Mysqlx.ServerMessages_OK))
			}
		}
	}
	debug.Msg("WaitingForAuthenticateOk: out of loop, return no error")
	return nil
}

func printableMsgTypeIn(i Mysqlx.ServerMessages_Type) string {
	return fmt.Sprintf("%d [%s]", i, Mysqlx.ServerMessages_Type_name[int32(i)])
}

func printableMsgTypeOut(i Mysqlx.ClientMessages_Type) string {
	return fmt.Sprintf("%d [%s]", i, Mysqlx.ClientMessages_Type_name[int32(i)])
}

// FIXME - there must be a protobuf function I can call - FIXME

func noticeTypeToName(t uint32) string {
	if name, found := mysqlxNoticeTypeName[t]; found {
		return name
	}
	return "?"
}

// Gets the value of the given MySQL System Variable
// The returned byte slice is only valid until the next read
func getSystemVarXProtocol(name string, mc *mysqlXConn) ([]byte, error) {
	debug.Msg("getSystemVarXProtocol() not implemented")

	return nil, nil
}

// write a StmtExecute packet with the given query
func (mc *mysqlXConn) writeStmtExecute(stmtExecute *Mysqlx_Sql.StmtExecute) error {
	var err error

	pb := new(netProtobuf)
	pb.msgType = int(Mysqlx.ClientMessages_SQL_STMT_EXECUTE)
	pb.payload, err = proto.Marshal(stmtExecute)

	if err != nil {
		log.Fatalf("Failed to marshall message: %+v: %v", stmtExecute, err)
	}

	err = mc.writeProtobufPacket(pb)
	if err != nil {
		return err
	}

	return nil
}

// Send a close message - no data to send so don't expose the protobuf info
func (mc *mysqlXConn) writeClose() error {
	payload, err := proto.Marshal(new(Mysqlx_Session.Close))
	if err != nil {
		return fmt.Errorf("mysqlXConn.writeClose: Failed to marshall close message: %v", err)
	}
	pb := &netProtobuf{
		msgType: int(Mysqlx.ClientMessages_SESS_CLOSE),
		payload: payload,
	}

	err = mc.writeProtobufPacket(pb)
	if err != nil {
		return fmt.Errorf("mysqlXConn.writeClose: Failed to write message: %v", err)
	}

	return nil
}

// show the error msg and eat it up
func (mc *mysqlXConn) processErrorMsg() error {
	if mc == nil {
		return fmt.Errorf("processErrorMsg mc == nil")
	}
	if mc.pb == nil {
		return fmt.Errorf("processErrorMsg mc.pb == nil")
	}
	if mc.pb.payload == nil {
		return fmt.Errorf("processErrorMsg mc.pb.payload == nil")
	}
	e := new(Mysqlx.Error)
	if err := proto.Unmarshal(mc.pb.payload, e); err != nil {
		return fmt.Errorf("unmarshaling error with e: %v", err)
	}
	debug.Msg("processErrorMsg: %v: ", errorText(e))
	mc.pb = nil

	return nil
}

// is this data printable?
func isPrintable(b []byte) bool {
	p := true
	for i := range b {
		if b[i] < 32 || b[i] > 126 {
			return false
		}
	}
	return p
}
