package main

/*
#cgo CFLAGS: -Ifdw -Ifdw/include/postgresql/server -Ifdw/include/postgresql/internal
#cgo linux LDFLAGS: -Wl,-unresolved-symbols=ignore-all
#cgo darwin LDFLAGS: -Wl,-undefined,dynamic_lookup
#include "fdw_helpers.h"
*/
import "C"

import (
	"fmt"
	"log"
	"net"
	"unsafe"

	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/turbot/steampipe-plugin-sdk/grpc"
	"github.com/turbot/steampipe-plugin-sdk/grpc/proto"
)

func RestrictionsToQuals(node *C.ForeignScanState, cinfos **C.ConversionInfo) []*proto.Qual {
	plan := (*C.ForeignScan)(unsafe.Pointer(node.ss.ps.plan))
	restrictions := plan.fdw_exprs

	var qualsList qualList
	if restrictions == nil {
		return qualsList.quals
	}

	for it := restrictions.head; it != nil; it = it.next {
		restriction := C.cellGetExpr(it)

		log.Printf("[TRACE] RestrictionsToQuals: restriction %s", C.GoString(C.tagTypeToString(C.fdw_nodeTag(restriction))))

		switch C.fdw_nodeTag(restriction) {
		case C.T_OpExpr:
			if q := qualFromOpExpr(C.cellGetOpExpr(it), node, cinfos); q != nil {
				qualsList.append(q)
			}
		case C.T_Var:
			q := qualFromVar(C.cellGetVar(it), node, cinfos)
			qualsList.append(q)

		case C.T_ScalarArrayOpExpr:
			if q := qualFromScalarOpExpr(C.cellGetScalarArrayOpExpr(it), node, cinfos); q != nil {
				qualsList.append(q)
			}
		case C.T_NullTest:
			q := qualFromNullTest(C.cellGetNullTest(it), node, cinfos)
			qualsList.append(q)
			//extractClauseFromNullTest(base_relids,				(NullTest *) node, qualsList);
		case C.T_BooleanTest:
			q := qualFromBooleanTest((*C.BooleanTest)(unsafe.Pointer(restriction)), node, cinfos)
			qualsList.append(q)
			//case C.T_BoolExpr:
			//	if q := qualFromBoolExpr((*C.BoolExpr)(unsafe.Pointer(restriction)), node, cinfos); q != nil {
			//		qualsList.append(q)
			//	}
		}

	}
	log.Printf("[INFO] RestrictionsToQuals: converted postgres restrictions protobuf quals")
	//for _, q := range qualsList.quals {
	//	log.Printf("[INFO] %s", grpc.QualToString(q))
	//}
	return qualsList.quals
}

// build a protobuf qual from an OpExpr
func qualFromOpExpr(restriction *C.OpExpr, node *C.ForeignScanState, cinfos **C.ConversionInfo) *proto.Qual {
	plan := (*C.ForeignScan)(unsafe.Pointer(node.ss.ps.plan))
	relids := C.bms_make_singleton(C.int(plan.scan.scanrelid))

	restriction = C.canonicalOpExpr(restriction, relids)
	if restriction == nil {
		log.Printf("[WARN] could not convert OpExpr to canonical form - NOT adding qual for OpExpr")
		return nil
	}

	left := (*C.Var)(C.list_nth(restriction.args, 0))
	right := C.list_nth(restriction.args, 1)

	// Do not add it if it either contains a mutable function, or makes self references in the right hand side.
	if C.contain_volatile_functions((*C.Node)(right)) || C.bms_is_subset(relids, C.pull_varnos((*C.Node)(right))) {
		log.Printf("[WARN] restriction either contains a mutable function, or makes self references in the right hand side - NOT adding qual for OpExpr")
		return nil
	}

	arrayIndex := left.varattno - 1
	ci := C.getConversionInfo(cinfos, C.int(arrayIndex))
	qualValue, err := getQualValue(right, node, ci)
	if err != nil {
		log.Printf("[INFO] failed to convert qual value; %v", err)
		return nil
	}

	column := C.GoString(ci.attrname)
	operatorName := C.GoString(C.getOperatorString(restriction.opno))
	qual := &proto.Qual{
		FieldName: column,
		Operator:  &proto.Qual_StringValue{StringValue: operatorName},
		Value:     qualValue,
	}

	log.Printf("[TRACE] qualFromOpExpr returning %v", qual)
	return qual
}

// build a protobuf qual from a Var - this converts to a simple boolean qual where column=true
func qualFromVar(arg *C.Var, node *C.ForeignScanState, cinfos **C.ConversionInfo) *proto.Qual {
	return &proto.Qual{
		FieldName: columnFromVar(arg, cinfos),
		Operator:  &proto.Qual_StringValue{StringValue: "="},
		Value:     &proto.QualValue{Value: &proto.QualValue_BoolValue{BoolValue: true}},
	}
}

func qualFromScalarOpExpr(restriction *C.ScalarArrayOpExpr, node *C.ForeignScanState, cinfos **C.ConversionInfo) *proto.Qual {
	plan := (*C.ForeignScan)(unsafe.Pointer(node.ss.ps.plan))
	relids := C.bms_make_singleton(C.int(plan.scan.scanrelid))

	restriction = C.canonicalScalarArrayOpExpr(restriction, relids)

	if restriction == nil {
		log.Printf("[WARN] could not convert OpExpr to canonical form - NOT adding qual for OpExpr")
		return nil
	}

	left := (*C.Var)(C.list_nth(restriction.args, 0))
	right := C.list_nth(restriction.args, 1)

	// Do not add it if it either contains a mutable function, or makes self references in the right hand side.
	if C.contain_volatile_functions((*C.Node)(right)) || C.bms_is_subset(relids, C.pull_varnos((*C.Node)(right))) {
		log.Printf("[WARN] restriction either contains a mutable function, or makes self references in the right hand side - NOT adding qual for OpExpr")
		return nil
	}

	arrayIndex := left.varattno - 1
	ci := C.getConversionInfo(cinfos, C.int(arrayIndex))
	qualValue, err := getQualValue(right, node, ci)
	if err != nil {
		log.Printf("[INFO] failed to convert qual value; %v", err)
		return nil
	}

	column := C.GoString(ci.attrname)
	operatorName := C.GoString(C.getOperatorString(restriction.opno))
	qual := &proto.Qual{
		FieldName: column,
		Operator:  &proto.Qual_StringValue{StringValue: operatorName},
		Value:     qualValue,
	}

	return qual
}

// build a protobuf qual from a NullTest
func qualFromNullTest(restriction *C.NullTest, node *C.ForeignScanState, cinfos **C.ConversionInfo) *proto.Qual {

	if C.fdw_nodeTag(restriction.arg) != C.T_Var {
		return nil
	}

	arg := (*C.Var)(unsafe.Pointer(restriction.arg))
	if arg.varattno < 1 {
		return nil
	}

	operatorName := ""
	if restriction.nulltesttype == C.IS_NULL {
		operatorName = "="
	} else {
		operatorName = "<>"
	}

	column := columnFromVar(arg, cinfos)

	qual := &proto.Qual{
		FieldName: column,
		Operator:  &proto.Qual_StringValue{StringValue: operatorName},
		Value:     nil,
	}
	return qual
}

// build a protobuf qual from a BoolTest
func qualFromBooleanTest(restriction *C.BooleanTest, node *C.ForeignScanState, cinfos **C.ConversionInfo) *proto.Qual {
	arg := restriction.arg
	if C.fdw_nodeTag(arg) != C.T_Var {
		return nil
	}

	variable := (*C.Var)(unsafe.Pointer(arg))
	column := columnFromVar(variable, cinfos)
	operatorName := ""

	switch restriction.booltesttype {
	case C.IS_TRUE:
		operatorName = "="

	case C.IS_NOT_TRUE, C.IS_FALSE:
		operatorName = "<>"
	default:
		return nil
	}

	qual := &proto.Qual{
		FieldName: column,
		Operator:  &proto.Qual_StringValue{StringValue: operatorName},
		Value:     &proto.QualValue{Value: &proto.QualValue_BoolValue{BoolValue: true}},
	}

	return qual
}

func columnFromVar(variable *C.Var, cinfos **C.ConversionInfo) string {
	arrayIndex := variable.varattno - 1
	ci := C.getConversionInfo(cinfos, C.int(arrayIndex))
	column := C.GoString(ci.attrname)
	return column
}

func qualFromBoolExpr(restriction *C.BoolExpr, node *C.ForeignScanState, cinfos **C.ConversionInfo) *proto.Qual {

	return nil
}

func getQualValue(right unsafe.Pointer, node *C.ForeignScanState, ci *C.ConversionInfo) (*proto.QualValue, error) {
	log.Printf("[TRACE] getQualValue")
	var isNull C.bool
	var typeOid C.Oid
	var value C.Datum
	valueExpression := (*C.Expr)(right)
	switch C.fdw_nodeTag(valueExpression) {
	case C.T_Const:
		constQual := (*C.Const)(right)
		typeOid = constQual.consttype
		value = constQual.constvalue
		isNull = constQual.constisnull
		log.Printf("[TRACE] getQualValue T_Const qual, value %v", value)
	case C.T_Param:
		paramQual := (*C.Param)(right)
		typeOid = paramQual.paramtype

		exprState := C.ExecInitExpr(valueExpression, (*C.PlanState)(unsafe.Pointer(node)))
		econtext := node.ss.ps.ps_ExprContext
		value = C.ExecEvalExpr(exprState, econtext, &isNull)
		log.Printf("[TRACE] getQualValue T_Param qual, value %v, isNull %v", value, isNull)
	default:
		return nil, fmt.Errorf("QualDefsToQuals: non-const qual value (type %s), skipping\n", C.GoString(C.tagTypeToString(C.fdw_nodeTag(valueExpression))))
	}

	var qualValue *proto.QualValue
	if isNull {
		log.Printf("[DEBUG] qualDef.isnull=true - returning qual with nil value")
		qualValue = nil
	} else {
		if typeOid == C.InvalidOid {
			typeOid = ci.atttypoid
		}
		var err error
		if qualValue, err = datumToQualValue(value, typeOid, ci); err != nil {
			return nil, err
		}
	}
	return qualValue, nil
}

func datumToQualValue(datum C.Datum, typeOid C.Oid, cinfo *C.ConversionInfo) (result *proto.QualValue, err error) {
	/* we support these postgres column types (see sqlTypeForColumnType):
	 bool
	 bigint
	 double precision
	 text
	 inet
	 cidr
	 jsonb
	 timestamp

	so we must handle quals of all these types

	*/
	result = &proto.QualValue{}
	switch typeOid {
	case C.TEXTOID, C.VARCHAROID:
		result.Value = &proto.QualValue_StringValue{StringValue: C.GoString(C.datumString(datum, cinfo))}
	case C.INETOID:
		// handle zero value - return nil
		if datum == 0 {
			break
		}

		inet := C.datumInet(datum, cinfo)
		ipAddrBytes := C.GoBytes(unsafe.Pointer(C.ipAddr(inet)), 16)
		netmaskBits := int32(C.netmaskBits(inet))
		var ipAddrString string
		var protocolVersion string
		if C.isIpV6(inet) {
			ipAddrString = net.IP(ipAddrBytes).String()
			protocolVersion = grpc.IPv6
			log.Printf("[TRACE] ipv6 qual: %s/%d", ipAddrString, netmaskBits)
		} else {
			ipAddrString = net.IPv4(ipAddrBytes[0], ipAddrBytes[1], ipAddrBytes[2], ipAddrBytes[3]).String()
			protocolVersion = grpc.IPv4
			log.Printf("[TRACE] ipv4 qual: %s/%d", ipAddrString, netmaskBits)
		}
		result.Value = &proto.QualValue_InetValue{
			InetValue: &proto.Inet{
				Mask:            netmaskBits,
				Addr:            ipAddrString,
				Cidr:            fmt.Sprintf("%s/%d", ipAddrString, netmaskBits),
				ProtocolVersion: protocolVersion,
			},
		}
	case C.DATEOID:
		pgts := int64(C.datumDate(datum, cinfo))
		var timestamp *timestamp.Timestamp
		timestamp, err := PgTimeToTimestamp(pgts)
		if err != nil {
			break
		}
		result.Value = &proto.QualValue_TimestampValue{TimestampValue: timestamp}
	case C.TIMESTAMPOID:
		pgts := int64(C.datumTimestamp(datum, cinfo))
		var timestamp *timestamp.Timestamp
		timestamp, err := PgTimeToTimestamp(pgts)
		if err != nil {
			break
		}
		result.Value = &proto.QualValue_TimestampValue{TimestampValue: timestamp}
	case C.INT2OID, C.INT4OID, C.INT8OID:
		result.Value = &proto.QualValue_Int64Value{Int64Value: int64(C.datumInt64(datum, cinfo))}
	case C.FLOAT4OID:
		result.Value = &proto.QualValue_DoubleValue{DoubleValue: float64(C.datumDouble(datum, cinfo))}
	case C.BOOLOID:
		result.Value = &proto.QualValue_BoolValue{BoolValue: bool(C.datumBool(datum, cinfo))}
	default:
		result, err = convertUnknown(datum, typeOid, cinfo)
	}
	return
}

func convertUnknown(datum C.Datum, typeOid C.Oid, cinfo *C.ConversionInfo) (*proto.QualValue, error) {
	tuple := C.fdw_searchSysCache1(C.TYPEOID, C.fdw_objectIdGetDatum(typeOid))
	if !C.fdw_heapTupleIsValid(tuple) {
		return nil, fmt.Errorf("lookup failed for type %v", typeOid)
	}
	typeStruct := (C.Form_pg_type)(unsafe.Pointer(C.fdw_getStruct(tuple)))
	C.ReleaseSysCache(tuple)

	if (typeStruct.typelem != 0) && (typeStruct.typlen == -1) {
		log.Printf("[TRACE] datum is an array")
		return datumArrayToQualValue(datum, typeOid, cinfo)
	}

	return nil, fmt.Errorf("Unknown qual type %v", typeOid)
}

func datumArrayToQualValue(datum C.Datum, typeOid C.Oid, cinfo *C.ConversionInfo) (*proto.QualValue, error) {
	iterator := C.array_create_iterator(C.fdw_datumGetArrayTypeP(datum), 0, nil)

	var qualValues []*proto.QualValue
	var elem C.Datum
	var isNull C.bool
	for C.array_iterate(iterator, &elem, &isNull) {
		if isNull == C.bool(true) {
			log.Printf("[TRACE] datumArrayToQualValue: null qual value: %v", isNull)
			log.Println(isNull)
			qualValues = append(qualValues, nil)
			continue
		}

		tuple := C.fdw_searchSysCache1(C.TYPEOID, C.fdw_objectIdGetDatum(typeOid))
		if !C.fdw_heapTupleIsValid(tuple) {
			return nil, fmt.Errorf("lookup failed for type %v", typeOid)
		}
		typeStruct := (C.Form_pg_type)(unsafe.Pointer(C.fdw_getStruct(tuple)))
		C.ReleaseSysCache(tuple)
		if qualValue, err := datumToQualValue(elem, typeStruct.typelem, cinfo); err != nil {
			return nil, err
		} else {
			log.Printf("[TRACE datumArrayToQualValue: successfully converted qual - adding qual value %v", qualValue)
			qualValues = append(qualValues, qualValue)
		}
	}
	var result = &proto.QualValue{
		Value: &proto.QualValue_ListValue{
			ListValue: &proto.QualValueList{
				Values: qualValues,
			},
		},
	}
	log.Printf("[TRACE] datumArrayToQualValue complete, returning array of %d quals values \n", len(qualValues))

	return result, nil
}

type qualList struct {
	quals []*proto.Qual
}

// append the qual to our list, checking for duplicates
func (q *qualList) append(qual *proto.Qual) {
	if !q.contains(qual) {
		q.quals = append(q.quals, qual)
	}
}

func (q *qualList) contains(other *proto.Qual) bool {
	for _, qual := range q.quals {
		if grpc.QualEquals(qual, other) {
			return true
		}
	}
	return false
}
