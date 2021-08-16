// Copyright 2016 - 2021 The excelize Authors. All rights reserved. Use of
// this source code is governed by a BSD-style license that can be found in
// the LICENSE file.
//
// Package excelize providing a set of functions that allow you to write to
// and read from XLSX / XLSM / XLTM files. Supports reading and writing
// spreadsheet documents generated by Microsoft Excel™ 2007 and later. Supports
// complex components by high compatibility, and provided streaming API for
// generating or reading data from a worksheet with huge amounts of data. This
// library needs Go version 1.15 or later.

package excelize

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"math"
	"strconv"

	"github.com/mohae/deepcopy"
)

// GetRows return all the rows in a sheet by given worksheet name
// (case sensitive). GetRows fetched the rows with value or formula cells,
// the tail continuously empty cell will be skipped. For example:
//
//    rows, err := f.GetRows("Sheet1")
//    if err != nil {
//        fmt.Println(err)
//        return
//    }
//    for _, row := range rows {
//        for _, colCell := range row {
//            fmt.Print(colCell, "\t")
//        }
//        fmt.Println()
//    }
//
func (f *File) GetRows(sheet string) ([][]string, error) {
	rows, err := f.Rows(sheet)
	if err != nil {
		return nil, err
	}
	results, cur, max := make([][]string, 0, 64), 0, 0
	for rows.Next() {
		cur++
		row, err := rows.Columns()
		if err != nil {
			break
		}
		results = append(results, row)
		if len(row) > 0 {
			max = cur
		}
	}
	return results[:max], nil
}

// Rows defines an iterator to a sheet.
type Rows struct {
	err                        error
	curRow, totalRow, stashRow int
	sheet                      string
	f                          *File
	decoder                    *xml.Decoder
}

// Next will return true if find the next row element.
func (rows *Rows) Next() bool {
	rows.curRow++
	return rows.curRow <= rows.totalRow
}

// Error will return the error when the error occurs.
func (rows *Rows) Error() error {
	return rows.err
}

// Columns return the current row's column values.
func (rows *Rows) Columns() ([]string, error) {
	var rowIterator rowXMLIterator
	if rows.stashRow >= rows.curRow {
		return rowIterator.columns, rowIterator.err
	}
	rowIterator.rows = rows
	rowIterator.d = rows.f.sharedStringsReader()
	for {
		token, _ := rows.decoder.Token()
		if token == nil {
			break
		}
		switch xmlElement := token.(type) {
		case xml.StartElement:
			rowIterator.inElement = xmlElement.Name.Local
			if rowIterator.inElement == "row" {
				rowIterator.row++
				if rowIterator.attrR, rowIterator.err = attrValToInt("r", xmlElement.Attr); rowIterator.attrR != 0 {
					rowIterator.row = rowIterator.attrR
				}
				if rowIterator.row > rowIterator.rows.curRow {
					rowIterator.rows.stashRow = rowIterator.row - 1
					return rowIterator.columns, rowIterator.err
				}
			}
			rowXMLHandler(&rowIterator, &xmlElement)
			if rowIterator.err != nil {
				return rowIterator.columns, rowIterator.err
			}
		case xml.EndElement:
			rowIterator.inElement = xmlElement.Name.Local
			if rowIterator.row == 0 {
				rowIterator.row = rowIterator.rows.curRow
			}
			if rowIterator.inElement == "row" && rowIterator.row+1 < rowIterator.rows.curRow {
				return rowIterator.columns, rowIterator.err
			}
			if rowIterator.inElement == "sheetData" {
				return rowIterator.columns, rowIterator.err
			}
		}
	}
	return rowIterator.columns, rowIterator.err
}

// appendSpace append blank characters to slice by given length and source slice.
func appendSpace(l int, s []string) []string {
	for i := 1; i < l; i++ {
		s = append(s, "")
	}
	return s
}

// ErrSheetNotExist defines an error of sheet is not exist
type ErrSheetNotExist struct {
	SheetName string
}

func (err ErrSheetNotExist) Error() string {
	return fmt.Sprintf("sheet %s is not exist", string(err.SheetName))
}

// rowXMLIterator defined runtime use field for the worksheet row SAX parser.
type rowXMLIterator struct {
	err                 error
	inElement           string
	attrR, cellCol, row int
	columns             []string
	rows                *Rows
	d                   *xlsxSST
}

// rowXMLHandler parse the row XML element of the worksheet.
func rowXMLHandler(rowIterator *rowXMLIterator, xmlElement *xml.StartElement) {
	rowIterator.err = nil
	if rowIterator.inElement == "c" {
		rowIterator.cellCol++
		colCell := xlsxC{}
		_ = rowIterator.rows.decoder.DecodeElement(&colCell, xmlElement)
		if colCell.R != "" {
			if rowIterator.cellCol, _, rowIterator.err = CellNameToCoordinates(colCell.R); rowIterator.err != nil {
				return
			}
		}
		blank := rowIterator.cellCol - len(rowIterator.columns)
		val, _ := colCell.getValueFrom(rowIterator.rows.f, rowIterator.d)
		if val != "" || colCell.F != nil {
			rowIterator.columns = append(appendSpace(blank, rowIterator.columns), val)
		}
	}
}

// Rows returns a rows iterator, used for streaming reading data for a
// worksheet with a large data. For example:
//
//    rows, err := f.Rows("Sheet1")
//    if err != nil {
//        fmt.Println(err)
//        return
//    }
//    for rows.Next() {
//        row, err := rows.Columns()
//        if err != nil {
//            fmt.Println(err)
//        }
//        for _, colCell := range row {
//            fmt.Print(colCell, "\t")
//        }
//        fmt.Println()
//    }
//
func (f *File) Rows(sheet string) (*Rows, error) {
	name, ok := f.sheetMap[trimSheetName(sheet)]
	if !ok {
		return nil, ErrSheetNotExist{sheet}
	}
	if ws, ok := f.Sheet.Load(name); ok && ws != nil {
		worksheet := ws.(*xlsxWorksheet)
		worksheet.Lock()
		defer worksheet.Unlock()
		// flush data
		output, _ := xml.Marshal(worksheet)
		f.saveFileList(name, f.replaceNameSpaceBytes(name, output))
	}
	var (
		err       error
		inElement string
		row       int
		rows      Rows
	)
	decoder := f.xmlNewDecoder(bytes.NewReader(f.readXML(name)))
	for {
		token, _ := decoder.Token()
		if token == nil {
			break
		}
		switch xmlElement := token.(type) {
		case xml.StartElement:
			inElement = xmlElement.Name.Local
			if inElement == "row" {
				row++
				for _, attr := range xmlElement.Attr {
					if attr.Name.Local == "r" {
						row, err = strconv.Atoi(attr.Value)
						if err != nil {
							return &rows, err
						}
					}
				}
				rows.totalRow = row
			}
		case xml.EndElement:
			if xmlElement.Name.Local == "sheetData" {
				rows.f = f
				rows.sheet = name
				rows.decoder = f.xmlNewDecoder(bytes.NewReader(f.readXML(name)))
				return &rows, nil
			}
		default:
		}
	}
	return &rows, nil
}

// SetRowHeight provides a function to set the height of a single row. For
// example, set the height of the first row in Sheet1:
//
//    err := f.SetRowHeight("Sheet1", 1, 50)
//
func (f *File) SetRowHeight(sheet string, row int, height float64) error {
	if row < 1 {
		return newInvalidRowNumberError(row)
	}
	if height > MaxRowHeight {
		return ErrMaxRowHeight
	}
	ws, err := f.workSheetReader(sheet)
	if err != nil {
		return err
	}

	prepareSheetXML(ws, 0, row)

	rowIdx := row - 1
	ws.SheetData.Row[rowIdx].Ht = height
	ws.SheetData.Row[rowIdx].CustomHeight = true
	return nil
}

// getRowHeight provides a function to get row height in pixels by given sheet
// name and row number.
func (f *File) getRowHeight(sheet string, row int) int {
	ws, _ := f.workSheetReader(sheet)
	ws.Lock()
	defer ws.Unlock()
	for i := range ws.SheetData.Row {
		v := &ws.SheetData.Row[i]
		if v.R == row && v.Ht != 0 {
			return int(convertRowHeightToPixels(v.Ht))
		}
	}
	// Optimisation for when the row heights haven't changed.
	return int(defaultRowHeightPixels)
}

// GetRowHeight provides a function to get row height by given worksheet name
// and row number. For example, get the height of the first row in Sheet1:
//
//    height, err := f.GetRowHeight("Sheet1", 1)
//
func (f *File) GetRowHeight(sheet string, row int) (float64, error) {
	if row < 1 {
		return defaultRowHeightPixels, newInvalidRowNumberError(row)
	}
	var ht = defaultRowHeight
	ws, err := f.workSheetReader(sheet)
	if err != nil {
		return ht, err
	}
	if ws.SheetFormatPr != nil && ws.SheetFormatPr.CustomHeight {
		ht = ws.SheetFormatPr.DefaultRowHeight
	}
	if row > len(ws.SheetData.Row) {
		return ht, nil // it will be better to use 0, but we take care with BC
	}
	for _, v := range ws.SheetData.Row {
		if v.R == row && v.Ht != 0 {
			return v.Ht, nil
		}
	}
	// Optimisation for when the row heights haven't changed.
	return ht, nil
}

// sharedStringsReader provides a function to get the pointer to the structure
// after deserialization of xl/sharedStrings.xml.
func (f *File) sharedStringsReader() *xlsxSST {
	var err error
	f.Lock()
	defer f.Unlock()
	relPath := f.getWorkbookRelsPath()
	if f.SharedStrings == nil {
		var sharedStrings xlsxSST
		ss := f.readXML("xl/sharedStrings.xml")
		if err = f.xmlNewDecoder(bytes.NewReader(namespaceStrictToTransitional(ss))).
			Decode(&sharedStrings); err != nil && err != io.EOF {
			log.Printf("xml decode error: %s", err)
		}
		if sharedStrings.Count == 0 {
			sharedStrings.Count = len(sharedStrings.SI)
		}
		if sharedStrings.UniqueCount == 0 {
			sharedStrings.UniqueCount = sharedStrings.Count
		}
		f.SharedStrings = &sharedStrings
		for i := range sharedStrings.SI {
			if sharedStrings.SI[i].T != nil {
				f.sharedStringsMap[sharedStrings.SI[i].T.Val] = i
			}
		}
		f.addContentTypePart(0, "sharedStrings")
		rels := f.relsReader(relPath)
		for _, rel := range rels.Relationships {
			if rel.Target == "/xl/sharedStrings.xml" {
				return f.SharedStrings
			}
		}
		// Update workbook.xml.rels
		f.addRels(relPath, SourceRelationshipSharedStrings, "/xl/sharedStrings.xml", "")
	}

	return f.SharedStrings
}

// getValueFrom return a value from a column/row cell, this function is
// inteded to be used with for range on rows an argument with the spreadsheet
// opened file.
func (c *xlsxC) getValueFrom(f *File, d *xlsxSST) (string, error) {
	f.Lock()
	defer f.Unlock()
	switch c.T {
	case "s":
		if c.V != "" {
			xlsxSI := 0
			xlsxSI, _ = strconv.Atoi(c.V)
			if len(d.SI) > xlsxSI {
				return f.formattedValue(c.S, d.SI[xlsxSI].String()), nil
			}
		}
		return f.formattedValue(c.S, c.V), nil
	case "str":
		return f.formattedValue(c.S, c.V), nil
	case "inlineStr":
		if c.IS != nil {
			return f.formattedValue(c.S, c.IS.String()), nil
		}
		return f.formattedValue(c.S, c.V), nil
	default:
		isNum, precision := isNumeric(c.V)
		if isNum && precision > 10 {
			val, _ := roundPrecision(c.V)
			if val != c.V {
				return f.formattedValue(c.S, val), nil
			}
		}
		return f.formattedValue(c.S, c.V), nil
	}
}

// roundPrecision round precision for numeric.
func roundPrecision(value string) (result string, err error) {
	var num float64
	if num, err = strconv.ParseFloat(value, 64); err != nil {
		return
	}
	result = fmt.Sprintf("%g", math.Round(num*numericPrecision)/numericPrecision)
	return
}

// SetRowVisible provides a function to set visible of a single row by given
// worksheet name and Excel row number. For example, hide row 2 in Sheet1:
//
//    err := f.SetRowVisible("Sheet1", 2, false)
//
func (f *File) SetRowVisible(sheet string, row int, visible bool) error {
	if row < 1 {
		return newInvalidRowNumberError(row)
	}

	ws, err := f.workSheetReader(sheet)
	if err != nil {
		return err
	}
	prepareSheetXML(ws, 0, row)
	ws.SheetData.Row[row-1].Hidden = !visible
	return nil
}

// GetRowVisible provides a function to get visible of a single row by given
// worksheet name and Excel row number. For example, get visible state of row
// 2 in Sheet1:
//
//    visible, err := f.GetRowVisible("Sheet1", 2)
//
func (f *File) GetRowVisible(sheet string, row int) (bool, error) {
	if row < 1 {
		return false, newInvalidRowNumberError(row)
	}

	ws, err := f.workSheetReader(sheet)
	if err != nil {
		return false, err
	}
	if row > len(ws.SheetData.Row) {
		return false, nil
	}
	return !ws.SheetData.Row[row-1].Hidden, nil
}

// SetRowOutlineLevel provides a function to set outline level number of a
// single row by given worksheet name and Excel row number. The value of
// parameter 'level' is 1-7. For example, outline row 2 in Sheet1 to level 1:
//
//    err := f.SetRowOutlineLevel("Sheet1", 2, 1)
//
func (f *File) SetRowOutlineLevel(sheet string, row int, level uint8) error {
	if row < 1 {
		return newInvalidRowNumberError(row)
	}
	if level > 7 || level < 1 {
		return ErrOutlineLevel
	}
	ws, err := f.workSheetReader(sheet)
	if err != nil {
		return err
	}
	prepareSheetXML(ws, 0, row)
	ws.SheetData.Row[row-1].OutlineLevel = level
	return nil
}

// GetRowOutlineLevel provides a function to get outline level number of a
// single row by given worksheet name and Excel row number. For example, get
// outline number of row 2 in Sheet1:
//
//    level, err := f.GetRowOutlineLevel("Sheet1", 2)
//
func (f *File) GetRowOutlineLevel(sheet string, row int) (uint8, error) {
	if row < 1 {
		return 0, newInvalidRowNumberError(row)
	}
	ws, err := f.workSheetReader(sheet)
	if err != nil {
		return 0, err
	}
	if row > len(ws.SheetData.Row) {
		return 0, nil
	}
	return ws.SheetData.Row[row-1].OutlineLevel, nil
}

// RemoveRow provides a function to remove single row by given worksheet name
// and Excel row number. For example, remove row 3 in Sheet1:
//
//    err := f.RemoveRow("Sheet1", 3)
//
// Use this method with caution, which will affect changes in references such
// as formulas, charts, and so on. If there is any referenced value of the
// worksheet, it will cause a file error when you open it. The excelize only
// partially updates these references currently.
func (f *File) RemoveRow(sheet string, row int) error {
	if row < 1 {
		return newInvalidRowNumberError(row)
	}

	ws, err := f.workSheetReader(sheet)
	if err != nil {
		return err
	}
	if row > len(ws.SheetData.Row) {
		return f.adjustHelper(sheet, rows, row, -1)
	}
	keep := 0
	for rowIdx := 0; rowIdx < len(ws.SheetData.Row); rowIdx++ {
		v := &ws.SheetData.Row[rowIdx]
		if v.R != row {
			ws.SheetData.Row[keep] = *v
			keep++
		}
	}
	ws.SheetData.Row = ws.SheetData.Row[:keep]
	return f.adjustHelper(sheet, rows, row, -1)
}

// InsertRow provides a function to insert a new row after given Excel row
// number starting from 1. For example, create a new row before row 3 in
// Sheet1:
//
//    err := f.InsertRow("Sheet1", 3)
//
// Use this method with caution, which will affect changes in references such
// as formulas, charts, and so on. If there is any referenced value of the
// worksheet, it will cause a file error when you open it. The excelize only
// partially updates these references currently.
func (f *File) InsertRow(sheet string, row int) error {
	if row < 1 {
		return newInvalidRowNumberError(row)
	}
	return f.adjustHelper(sheet, rows, row, 1)
}

// DuplicateRow inserts a copy of specified row (by its Excel row number) below
//
//    err := f.DuplicateRow("Sheet1", 2)
//
// Use this method with caution, which will affect changes in references such
// as formulas, charts, and so on. If there is any referenced value of the
// worksheet, it will cause a file error when you open it. The excelize only
// partially updates these references currently.
func (f *File) DuplicateRow(sheet string, row int) error {
	return f.DuplicateRowTo(sheet, row, row+1)
}

// DuplicateRowTo inserts a copy of specified row by it Excel number
// to specified row position moving down exists rows after target position
//
//    err := f.DuplicateRowTo("Sheet1", 2, 7)
//
// Use this method with caution, which will affect changes in references such
// as formulas, charts, and so on. If there is any referenced value of the
// worksheet, it will cause a file error when you open it. The excelize only
// partially updates these references currently.
func (f *File) DuplicateRowTo(sheet string, row, row2 int) error {
	if row < 1 {
		return newInvalidRowNumberError(row)
	}

	ws, err := f.workSheetReader(sheet)
	if err != nil {
		return err
	}
	if row > len(ws.SheetData.Row) || row2 < 1 || row == row2 {
		return nil
	}

	var ok bool
	var rowCopy xlsxRow

	for i, r := range ws.SheetData.Row {
		if r.R == row {
			rowCopy = deepcopy.Copy(ws.SheetData.Row[i]).(xlsxRow)
			ok = true
			break
		}
	}
	if !ok {
		return nil
	}

	if err := f.adjustHelper(sheet, rows, row2, 1); err != nil {
		return err
	}

	idx2 := -1
	for i, r := range ws.SheetData.Row {
		if r.R == row2 {
			idx2 = i
			break
		}
	}
	if idx2 == -1 && len(ws.SheetData.Row) >= row2 {
		return nil
	}

	rowCopy.C = append(make([]xlsxC, 0, len(rowCopy.C)), rowCopy.C...)
	f.ajustSingleRowDimensions(&rowCopy, row2)

	if idx2 != -1 {
		ws.SheetData.Row[idx2] = rowCopy
	} else {
		ws.SheetData.Row = append(ws.SheetData.Row, rowCopy)
	}
	return f.duplicateMergeCells(sheet, ws, row, row2)
}

// duplicateMergeCells merge cells in the destination row if there are single
// row merged cells in the copied row.
func (f *File) duplicateMergeCells(sheet string, ws *xlsxWorksheet, row, row2 int) error {
	if ws.MergeCells == nil {
		return nil
	}
	if row > row2 {
		row++
	}
	for _, rng := range ws.MergeCells.Cells {
		coordinates, err := areaRefToCoordinates(rng.Ref)
		if err != nil {
			return err
		}
		if coordinates[1] < row2 && row2 < coordinates[3] {
			return nil
		}
	}
	for i := 0; i < len(ws.MergeCells.Cells); i++ {
		areaData := ws.MergeCells.Cells[i]
		coordinates, _ := areaRefToCoordinates(areaData.Ref)
		x1, y1, x2, y2 := coordinates[0], coordinates[1], coordinates[2], coordinates[3]
		if y1 == y2 && y1 == row {
			from, _ := CoordinatesToCellName(x1, row2)
			to, _ := CoordinatesToCellName(x2, row2)
			if err := f.MergeCell(sheet, from, to); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkRow provides a function to check and fill each column element for all
// rows and make that is continuous in a worksheet of XML. For example:
//
//    <row r="15" spans="1:22" x14ac:dyDescent="0.2">
//        <c r="A15" s="2" />
//        <c r="B15" s="2" />
//        <c r="F15" s="1" />
//        <c r="G15" s="1" />
//    </row>
//
// in this case, we should to change it to
//
//    <row r="15" spans="1:22" x14ac:dyDescent="0.2">
//        <c r="A15" s="2" />
//        <c r="B15" s="2" />
//        <c r="C15" s="2" />
//        <c r="D15" s="2" />
//        <c r="E15" s="2" />
//        <c r="F15" s="1" />
//        <c r="G15" s="1" />
//    </row>
//
// Noteice: this method could be very slow for large spreadsheets (more than
// 3000 rows one sheet).
func checkRow(ws *xlsxWorksheet) error {
	for rowIdx := range ws.SheetData.Row {
		rowData := &ws.SheetData.Row[rowIdx]

		colCount := len(rowData.C)
		if colCount == 0 {
			continue
		}
		// check and fill the cell without r attribute in a row element
		rCount := 0
		for idx, cell := range rowData.C {
			rCount++
			if cell.R != "" {
				lastR, _, err := CellNameToCoordinates(cell.R)
				if err != nil {
					return err
				}
				if lastR > rCount {
					rCount = lastR
				}
				continue
			}
			rowData.C[idx].R, _ = CoordinatesToCellName(rCount, rowIdx+1)
		}
		lastCol, _, err := CellNameToCoordinates(rowData.C[colCount-1].R)
		if err != nil {
			return err
		}

		if colCount < lastCol {
			oldList := rowData.C
			newlist := make([]xlsxC, 0, lastCol)

			rowData.C = ws.SheetData.Row[rowIdx].C[:0]

			for colIdx := 0; colIdx < lastCol; colIdx++ {
				cellName, err := CoordinatesToCellName(colIdx+1, rowIdx+1)
				if err != nil {
					return err
				}
				newlist = append(newlist, xlsxC{R: cellName})
			}

			rowData.C = newlist

			for colIdx := range oldList {
				colData := &oldList[colIdx]
				colNum, _, err := CellNameToCoordinates(colData.R)
				if err != nil {
					return err
				}
				ws.SheetData.Row[rowIdx].C[colNum-1] = *colData
			}
		}
	}
	return nil
}

// SetRowStyle provides a function to set style of rows by given worksheet
// name, row range and style ID. Note that this will overwrite the existing
// styles for the cell, it won't append or merge style with existing styles.
//
// For example set style of row 1 on Sheet1:
//
//    err = f.SetRowStyle("Sheet1", 1, style)
//
// Set style of rows 1 to 10 on Sheet1:
//
//    err = f.SetRowStyle("Sheet1", 1, 10, style)
//
func (f *File) SetRowStyle(sheet string, start, end, styleID int) error {
	if end < start {
		start, end = end, start
	}
	if start < 1 {
		return newInvalidRowNumberError(start)
	}
	if end > TotalRows {
		return ErrMaxRows
	}
	if styleID < 0 {
		return newInvalidStyleID(styleID)
	}
	ws, err := f.workSheetReader(sheet)
	if err != nil {
		return err
	}
	prepareSheetXML(ws, 0, end)
	for row := start - 1; row < end; row++ {
		ws.SheetData.Row[row].S = styleID
		ws.SheetData.Row[row].CustomFormat = true
	}
	return nil
}

// convertRowHeightToPixels provides a function to convert the height of a
// cell from user's units to pixels. If the height hasn't been set by the user
// we use the default value. If the row is hidden it has a value of zero.
func convertRowHeightToPixels(height float64) float64 {
	var pixels float64
	if height == 0 {
		return pixels
	}
	pixels = math.Ceil(4.0 / 3.0 * height)
	return pixels
}
