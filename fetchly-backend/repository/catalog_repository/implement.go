package catalogrepository

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"

	"github.com/fetchlydev/source/fetchly-backend/config"
	"github.com/fetchlydev/source/fetchly-backend/core/entity"
	repository_intf "github.com/fetchlydev/source/fetchly-backend/core/repository"
	"github.com/fetchlydev/source/fetchly-backend/pkg/helper"
	"github.com/fetchlydev/source/fetchly-backend/repository/util"
	"gorm.io/gorm"
)

type repository struct {
	cfg config.Config
	db  *gorm.DB
}

func New(cfg config.Config, db *gorm.DB) repository_intf.CatalogRepository {
	return &repository{
		cfg: cfg,
		db:  db,
	}
}

func (r *repository) GetColumnList(ctx context.Context, request entity.CatalogQuery) (columns []map[string]any, columnStrings string, joinQueryMap map[string]string, joinQueryOrder []string, err error) {
	joinQueryMapAll := make(map[string]string)
	joinQueryOrderAll := make([]string, 0)

	// get list of column from request.ObjectCode
	listColumnQuery := fmt.Sprintf(`
	SELECT
    col.column_name as field_code, 
		col.udt_name as data_type,
    ccu.table_name AS foreign_table_name,
    ccu.column_name AS foreign_field_name
	FROM
			information_schema.columns AS col
	LEFT JOIN information_schema.key_column_usage AS kcu ON col.table_name = kcu.table_name
	AND col.column_name = kcu.column_name
	LEFT JOIN information_schema.constraint_column_usage AS ccu ON kcu.constraint_name = ccu.constraint_name
	WHERE
		col.table_schema = '%v' 
	AND col.table_name = '%v'
	`, request.TenantCode, request.ObjectCode)

	db := r.db

	if r.cfg.IsDebugMode {
		db.Debug()
	}

	rows, err := db.Raw(listColumnQuery).Rows()
	if err != nil {
		return columns, columnStrings, joinQueryMap, joinQueryOrder, err
	}
	defer rows.Close()

	// iterate over the result to get value of column_name and data_type
	for rows.Next() {
		column := make(map[string]any)

		var columnCode, dataType, foreignTableName, foreignColumnName interface{}
		if err := rows.Scan(&columnCode, &dataType, &foreignTableName, &foreignColumnName); err != nil {
			return columns, columnStrings, joinQueryMap, joinQueryOrder, err
		}

		column[entity.FieldDataType] = dataType.(string)
		column[entity.FieldColumnCode] = columnCode.(string)
		column[entity.FieldColumnName] = columnCode.(string)
		column[entity.FieldCompleteColumnCode] = fmt.Sprintf("%v.%v.%v", request.TenantCode, request.ObjectCode, columnCode.(string))

		if foreignTableName != nil && foreignTableName.(string) != request.ObjectCode && foreignColumnName != nil && foreignColumnName.(string) != "id" {
			column[entity.FieldForeignTableName] = foreignTableName.(string)
			column[entity.FieldForeignColumnName] = foreignColumnName.(string)
		}

		columns = append(columns, column)
	}

	for _, column := range columns {
		_, foreignTableNameOK := column["foreign_table_name"]
		_, foreignFieldNameOK := column["foreign_field_name"]

		if foreignTableNameOK && foreignFieldNameOK {
			// append foreign name display field into select
			// room for improvement: replace hardcoded __name with display value from catalog
			newForeignColumn := make(map[string]any)
			foreignColumName := fmt.Sprintf("%v__name", column["field_code"])

			newForeignColumn[entity.FieldDataType] = "string"
			newForeignColumn[entity.FieldColumnCode] = foreignColumName
			newForeignColumn[entity.FieldColumnName] = foreignColumName
			newForeignColumn[entity.FieldCompleteColumnCode] = fmt.Sprintf("%v", foreignColumName)

			columns = append(columns, newForeignColumn)
		}
	}

	// filter columns if request.Fields is not empty
	if len(request.Fields) > 0 {
		var filteredColumns []map[string]any
		for fieldNameKey, fieldValue := range request.Fields {
			isFound := false

			if !strings.Contains(fieldNameKey, "__") {
				for _, column := range columns {
					if fieldNameKey == column[entity.FieldColumnCode] {
						isFound = true
						column[entity.FieldColumnCode] = fieldNameKey
						column[entity.FieldIsDisplayedInTable] = fieldValue.IsDisplayedInTable

						if fieldValue.FieldOrder != 0 {
							column[entity.FieldFieldOrder] = fieldValue.FieldOrder
						}

						if fieldValue.RenderConfig != "" {
							column[entity.FieldRenderConfig] = fieldValue.RenderConfig
						}

						filteredColumns = append(filteredColumns, column)
					}
				}
			} else {
				// handle fieldName that has double underscore this indicates that it is a relationship field
				r.handleJoinColumn(ctx, request, fieldNameKey, &joinQueryMapAll, &joinQueryOrderAll, &filteredColumns)
				isFound = true
			}

			if fieldValue.FieldCode != "" || fieldValue.FieldName != "" {
				for _, column := range filteredColumns {
					if fieldNameKey == column[entity.FieldColumnCode] && fieldValue.FieldCode != "" {
						column[entity.FieldColumnCode] = fieldValue.FieldCode
					}

					if fieldNameKey == column[entity.FieldColumnCode] && fieldValue.FieldName != "" {
						column[entity.FieldColumnName] = fieldValue.FieldName
					}
				}
			}

			// after finish iterating columns, if field is not found in columns, return error
			if !isFound {
				return columns, columnStrings, joinQueryMap, joinQueryOrder, fmt.Errorf("field %v is not found in table %v", fieldNameKey, request.ObjectCode)
			}
		}

		columns = filteredColumns
	} else {
		for _, col := range columns {
			completeFieldCode := col[entity.FieldCompleteColumnCode].(string)
			if strings.Contains(completeFieldCode, "__") {
				r.handleJoinColumn(ctx, request, completeFieldCode, &joinQueryMapAll, &joinQueryOrderAll, &columns)
			}
		}
	}

	// convert columns to string
	for i, col := range columns {
		completeFieldCode := col[entity.FieldCompleteColumnCode].(string)

		// convert into columnStrings
		if i == 0 {
			columnStrings = completeFieldCode
		} else {
			columnStrings = columnStrings + ", " + completeFieldCode
		}
	}

	return columns, columnStrings, joinQueryMapAll, joinQueryOrderAll, err
}

func (r *repository) handleJoinColumn(
	ctx context.Context,
	request entity.CatalogQuery,
	fieldNameKey string,
	joinQueryMapAll *map[string]string,
	joinQueryOrderAll *[]string,
	filteredColumns *[]map[string]any,
) {
	foreignFieldSet := strings.Split(fieldNameKey, "__")
	_, joinQueryMap := r.HandleChainingJoinQuery(ctx, "", fieldNameKey, fmt.Sprintf("%v.%v", request.TenantCode, request.ObjectCode), request, entity.FilterItem{})

	// append joinQueryMap to joinQueryMapAll
	for k, v := range joinQueryMap {
		(*joinQueryMapAll)[k] = v
		*joinQueryOrderAll = append(*joinQueryOrderAll, k)
	}

	// split fieldName by double underscore
	foreignColumnName := fmt.Sprintf("%v.%v.%v", request.TenantCode, request.ObjectCode, foreignFieldSet[0])
	referenceColumnName := foreignFieldSet[1]

	if _, ok := joinQueryMap[fieldNameKey]; ok {
		fieldNameKeyList := strings.Split(fieldNameKey, "__")
		destinationColumn := fieldNameKeyList[len(fieldNameKeyList)-1]

		fieldName := fmt.Sprintf("%v.%v", fieldNameKey, destinationColumn)
		fieldCode := fieldName

		if val := request.Fields[fieldNameKey].FieldName; val != "" {
			fieldName = val
		}

		filteredColumn := map[string]any{
			entity.FieldOriginalFieldCode:  fieldNameKey,
			entity.FieldCompleteColumnCode: fieldCode,
			entity.FieldColumnCode:         fieldCode,
			entity.FieldColumnName:         fieldName,
			entity.FieldForeignColumnName:  foreignColumnName,
			entity.FieldDataType:           "text",
			entity.ForeignTable: map[string]string{
				entity.FieldForeignColumnName: referenceColumnName,
			},
			entity.FieldIsDisplayedInTable: request.Fields[fieldNameKey].IsDisplayedInTable,
			entity.FieldFieldOrder:         request.Fields[fieldNameKey].FieldOrder,
			entity.FieldRenderConfig:       request.Fields[fieldNameKey].RenderConfig,
		}

		*filteredColumns = append(*filteredColumns, filteredColumn)
	}
}

func (r *repository) GetObjectData(ctx context.Context, request entity.CatalogQuery) (resp entity.CatalogResponse, err error) {
	// get list of column from request.ObjectCode
	completeTableName := request.TenantCode + "." + request.ObjectCode

	// Get list of columns
	columnsList, columnsString, joinQueryMap, joinQueryOrder, err := r.GetColumnList(ctx, request)
	if err != nil {
		return resp, err
	}

	// Get total data count
	countQuery := r.getTotalCountQuery(ctx, completeTableName, request, joinQueryMap, joinQueryOrder, columnsList)
	resultCount, err := r.db.Raw(countQuery).Rows()
	if err != nil {
		return resp, err
	}

	for resultCount.Next() {
		resultCount.Scan(&resp.TotalData)
	}

	if request.PageSize < 1 {
		request.PageSize = 10
	}

	if request.Page < 1 {
		request.Page = 1
	}

	// Get data with pagination
	dataQuery := r.getDataWithPagination(ctx, columnsString, completeTableName, request, joinQueryMap, joinQueryOrder, columnsList)
	rows, err := r.db.Raw(dataQuery).Rows()
	if err != nil {
		return resp, err
	}
	defer rows.Close()

	for rows.Next() {
		item, err := util.HandleSingleRow(columnsList, rows, request)
		if err != nil {
			return resp, err
		}

		// Append the item to the catalog
		resp.Items = append(resp.Items, item)
	}

	resp.Page = request.Page
	resp.PageSize = request.PageSize
	resp.TotalPage = int(helper.GenerateTotalPage(int64(resp.TotalData), int64(request.PageSize)))

	return resp, nil
}

func (r *repository) GetObjectDetail(ctx context.Context, request entity.CatalogQuery) (resp map[string]entity.DataItem, err error) {
	// get list of column from request.ObjectCode
	completeTableName := request.TenantCode + "." + request.ObjectCode

	// Get list of columns
	columnsList, columnsString, joinQueryMap, joinQueryOrder, err := r.GetColumnList(ctx, request)
	if err != nil {
		return resp, err
	}

	// get single data using serial in request
	dataQuery := r.getSingleData(ctx, columnsList, columnsString, completeTableName, request, joinQueryMap, joinQueryOrder)
	rows, err := r.db.Raw(dataQuery).Rows()
	if err != nil {
		return resp, err
	}

	for rows.Next() {
		item, err := util.HandleSingleRow(columnsList, rows, request)
		if err != nil {
			return resp, err
		}

		resp = item
	}

	return resp, nil
}

func (r *repository) GetDataByRawQuery(ctx context.Context, request entity.CatalogQuery) (resp entity.CatalogResponse, err error) {
	// run raw query from request.RawQuery
	rawQuery := request.RawQuery

	// get total data based on rawQuery
	rawCountQuery := fmt.Sprintf("SELECT SUM(1) as total from (%s) as subquery", rawQuery)

	countRows, err := r.db.Raw(rawCountQuery).Rows()
	if err != nil {
		return resp, err
	}
	defer countRows.Close()

	var total int
	if countRows.Next() {
		err = countRows.Scan(&total)
		if err != nil {
			return resp, err
		}

		resp.TotalData = total
	}

	// add page and page size based on request.Page and request.PageSize
	rawQuery = fmt.Sprintf("%s LIMIT %d OFFSET %d", rawQuery, request.PageSize, (request.Page-1)*request.PageSize)

	rows, err := r.db.Raw(rawQuery).Rows()
	if err != nil {
		return resp, err
	}
	defer rows.Close()

	// get list of column from query result
	columns, err := rows.Columns()
	if err != nil {
		return resp, err
	}

	// iterate over the result to get value of column_name and data_type
	for rows.Next() {
		// Create a slice of interface{} to hold column values
		values := make([]interface{}, len(columns))
		valuePointers := make([]interface{}, len(columns))
		for i := range values {
			valuePointers[i] = &values[i]
		}

		// Scan the row
		if err := rows.Scan(valuePointers...); err != nil {
			return resp, err
		}

		// Create a map for the row
		item := make(map[string]entity.DataItem)
		for i, col := range columns {
			item[col] = entity.DataItem{
				FieldCode:    col,
				FieldName:    helper.CapitalizeWords(helper.ReplaceUnderscoreWithSpace(col)),
				DataType:     "text",
				Value:        values[i],
				DisplayValue: values[i],
			}
		}

		// Append the item to the catalog
		resp.Items = append(resp.Items, item)
	}

	resp.Page = request.Page
	resp.PageSize = request.PageSize
	resp.TotalPage = int(helper.GenerateTotalPage(int64(resp.TotalData), int64(request.PageSize)))

	return resp, nil
}

func (r *repository) CreateObjectData(ctx context.Context, request entity.DataMutationRequest) (resp map[string]entity.DataItem, err error) {
	// INSERT INTO table_name (column1, column2, column3, ...)
	// VALUES (value1, value2, value3, ...);

	// get list of column from request.ObjectCode
	completeTableName := request.TenantCode + "." + request.ObjectCode

	// loop through data items and get the values
	var columnCodeString string
	var valueString string
	for _, item := range request.Items {
		columnCodeString = columnCodeString + ", " + item.FieldCode

		if item.Value == nil {
			valueString = valueString + ", NULL"
		} else {
			switch item.DataType {
			case "text":
				valueString = valueString + fmt.Sprintf(", '%v'", item.Value)
			case "integer":
				valueString = valueString + fmt.Sprintf(", %v", item.Value)
			case "boolean":
				valueString = valueString + fmt.Sprintf(", %v", item.Value)
			default:
				valueString = valueString + fmt.Sprintf(", '%v'", item.Value)
			}
		}
	}

	if len(valueString) == 0 {
		return resp, errors.New("no data item found")
	}

	columnCodeString = columnCodeString[2:]
	valueString = valueString[2:]

	// insert into query string
	insertQuery := fmt.Sprintf("INSERT INTO %v (%v) VALUES (%v)", completeTableName, columnCodeString, valueString)
	log.Printf("insertQuery: %v", insertQuery)

	// execute insert query
	if err := r.db.Exec(insertQuery).Error; err != nil {
		return resp, err
	}

	return resp, nil
}

func (r *repository) UpdateObjectData(ctx context.Context, request entity.DataMutationRequest) (resp map[string]entity.DataItem, err error) {
	// UPDATE table_name
	// SET column1 = value1, column2 = value2, ...
	// WHERE condition;

	// get list of column from request.ObjectCode
	columnList, _, _, _, err := r.GetColumnList(ctx, entity.CatalogQuery{
		ObjectCode:  request.ObjectCode,
		TenantCode:  request.TenantCode,
		ProductCode: request.ProductCode,
	})
	if err != nil {
		return resp, err
	}

	columnListMap := make(map[string]map[string]any)
	for _, column := range columnList {
		columnListMap[column[entity.FieldColumnCode].(string)] = column
	}

	// get mutation data from request
	mutationData := request.Items
	mutationDataMap := make(map[string]entity.DataItem)
	for _, item := range mutationData {
		mutationDataMap[item.FieldCode] = item
	}

	// get existing data using serial
	existingData, err := r.GetObjectDetail(ctx, entity.CatalogQuery{
		ObjectCode:  request.ObjectCode,
		TenantCode:  request.TenantCode,
		ProductCode: request.ProductCode,
		Serial:      request.Serial,
	})
	if err != nil {
		return resp, err
	}

	// compare mutationDataMap and existingDataMap using each column code respectively
	for key, existingItem := range existingData {
		if existingItem.Value == mutationDataMap[key].Value {
			// remove from mutationDataMap
			delete(mutationDataMap, key)
		}
	}

	if len(mutationDataMap) == 0 {
		return resp, entity.ErrorNoUpdateDataFound
	}

	// compose update query
	var updateQuery string
	for key, item := range mutationDataMap {
		if column, ok := columnListMap[key]; ok {
			if item.Value == nil {
				updateQuery = updateQuery + fmt.Sprintf("%v = NULL, ", column[entity.FieldColumnName])
			} else {
				switch strings.ToLower(column[entity.FieldDataType].(string)) {
				case "text", "varchar", "char", "json", "jsonb":
					updateQuery = updateQuery + fmt.Sprintf("%v = '%v', ", column[entity.FieldColumnName], item.Value)
				case "integer", "int", "bigint", "smallint":
					updateQuery = updateQuery + fmt.Sprintf("%v = %v, ", column[entity.FieldColumnName], item.Value)
				case "boolean", "bool":
					updateQuery = updateQuery + fmt.Sprintf("%v = %v, ", column[entity.FieldColumnName], item.Value)
				default:
					updateQuery = updateQuery + fmt.Sprintf("%v = '%v', ", column[entity.FieldColumnName], item.Value)
				}
			}
		}
	}

	// remove last comma and space
	if len(updateQuery) > 0 {
		updateQuery = updateQuery[:len(updateQuery)-2]
	}

	// compose where clause
	identifierColumn := entity.DEFAULT_IDENTIFIER
	if !helper.IsUUID(request.Serial) {
		identifierColumn = entity.DEFAULT_IDENTIFIER
	}

	// check if table has updated_at column
	// if yes, then add updated_at = now() to update query
	if _, ok := columnListMap["updated_at"]; ok {
		updateQuery = updateQuery + fmt.Sprintf(", %v = now()", columnListMap["updated_at"][entity.FieldColumnCode])
	}

	if _, ok := columnListMap["updated_by"]; ok {
		updateQuery = fmt.Sprintf("%v, %v = '%v'", updateQuery, columnListMap["updated_by"][entity.FieldColumnCode], request.UserSerial)
	}

	// compose update query
	completeTableName := request.TenantCode + "." + request.ObjectCode
	updateQuery = fmt.Sprintf("UPDATE %v SET %v WHERE %v.%v = '%v'", completeTableName, updateQuery, completeTableName, identifierColumn, request.Serial)

	// execute update query
	if err := r.db.Exec(updateQuery).Error; err != nil {
		return resp, err
	}

	// get updated data using serial
	updatedData, err := r.GetObjectDetail(ctx, entity.CatalogQuery{
		ObjectCode:  request.ObjectCode,
		TenantCode:  request.TenantCode,
		ProductCode: request.ProductCode,
		Serial:      request.Serial,
	})
	if err != nil {
		return resp, err
	}

	return updatedData, nil
}

func (r *repository) DeleteObjectData(ctx context.Context, request entity.DataMutationRequest) (err error) {
	// compose where clause
	identifierColumn := entity.DEFAULT_IDENTIFIER
	if !helper.IsUUID(request.Serial) {
		identifierColumn = entity.DEFAULT_IDENTIFIER
	}

	// compose delete query
	completeTableName := request.TenantCode + "." + request.ObjectCode
	updateQuery := fmt.Sprintf("UPDATE %v SET deleted_at = NOW() WHERE %v.%v = '%v'", completeTableName, completeTableName, identifierColumn, request.Serial)

	// execute update query
	if err := r.db.Exec(updateQuery).Error; err != nil {
		return err
	}

	return nil
}

func isOperatorInLIKEList(operator entity.FilterOperator) bool {
	for _, validOperator := range entity.OperatorLIKEList {
		if operator == validOperator {
			return true
		}
	}

	return false
}

func (r *repository) GetObjectByCode(ctx context.Context, objectCode, tenantCode string) (resp entity.Objects, err error) {
	db := r.db.Model(&Objects{})
	db.Joins("JOIN tenants ON tenants.serial = objects.tenant_serial")
	db.Where("objects.code = ?", objectCode)
	db.Where("tenants.code = ?", tenantCode)

	result := Objects{}
	if err := db.First(&result).Error; err != nil {
		return resp, err
	}

	return result.ToEntity(), nil
}

func (r *repository) GetObjectFieldsByObjectCode(ctx context.Context, request entity.CatalogQuery) (resp map[string]any, err error) {
	// get list of column from request.ObjectCode
	resp = make(map[string]any)

	db := r.db.Model(&ObjectFields{})

	if r.cfg.IsDebugMode {
		db = db.Debug()
	}

	results := []ObjectFields{}
	if err := db.Where("object_serial = ?", request.ObjectSerial).Find(&results).Error; err != nil {
		return resp, err
	}

	// iterate over the result to get value of column_name and data_type
	for _, result := range results {
		resp[result.FieldCode] = result.ToEntity()
	}

	return resp, nil
}

func (r *repository) GetDataTypeBySerial(ctx context.Context, serial string) (resp entity.DataType, err error) {
	db := r.db.Model(&DataType{})
	db.Where("serial = ?", serial)

	result := DataType{}
	if err := db.First(&result).Error; err != nil {
		return resp, err
	}

	return result.ToEntity(), nil
}

func (r *repository) GetDataTypeBySerials(ctx context.Context, serials []string) (resp []entity.DataType, err error) {
	if len(serials) == 0 {
		return resp, nil // Return an empty response if no serials are provided
	}

	db := r.db.Model(&DataType{})

	if r.cfg.IsDebugMode {
		db = db.Debug()
	}

	var results []DataType
	if err := db.Where("serial IN ?", serials).Find(&results).Error; err != nil {
		return resp, fmt.Errorf("failed to fetch data types: %w", err)
	}

	for _, result := range results {
		resp = append(resp, result.ToEntity())
	}

	return resp, nil
}

func (r *repository) GetForeignKeyInfo(ctx context.Context, tableName, columnName, schemaName string) (resp entity.ForeignKeyInfo, err error) {
	query := `
	SELECT
		ccu.table_schema AS foreign_schema,
		ccu.table_name   AS foreign_table,
		ccu.column_name  AS foreign_column
	FROM
		information_schema.table_constraints AS tc
		JOIN information_schema.key_column_usage AS kcu
		  ON tc.constraint_name = kcu.constraint_name
		 AND tc.constraint_schema = kcu.constraint_schema
		JOIN information_schema.constraint_column_usage AS ccu
		  ON ccu.constraint_name = tc.constraint_name
		 AND ccu.constraint_schema = tc.constraint_schema
	WHERE
		tc.constraint_type = 'FOREIGN KEY'
		AND kcu.column_name = ?
		AND tc.table_name = ?
		AND tc.table_schema = ?
	LIMIT 1;
	`

	result := ForeignKeyInfo{}
	if err = r.db.Raw(query, columnName, tableName, schemaName).Scan(&result).Error; err != nil {
		return resp, err
	}

	return result.ToEntity(), nil
}

// local function

// Helper function to build dynamic filters based on CatalogQuery
func (r *repository) buildFilters(_ context.Context, request entity.CatalogQuery) string {
	var filterClauses []string

	for _, filterGroup := range request.Filters {
		var groupClauses []string

		for fieldName, filter := range filterGroup.Filters {
			completeTableName := fmt.Sprintf("%v.%v", request.TenantCode, request.ObjectCode)
			operator := entity.OperatorQueryMap[filter.Operator]
			value := filter.Value

			// handler value of operator is part of entity.OperatorLIKEList, then we should add %
			if isOperatorInLIKEList(filter.Operator) {
				value = fmt.Sprintf("%%%v%%", value)
			}

			// Create filter conditions based on the field, operator, and value
			var formattedValue string

			switch v := value.(type) {
			case nil:
				formattedValue = "NULL"
			case string:
				formattedValue = fmt.Sprintf("'%s'", v)
			case bool:
				formattedValue = fmt.Sprintf("%t", v)
			case int, int8, int16, int32, int64:
				formattedValue = fmt.Sprintf("%d", v)
			case float32, float64:
				formattedValue = fmt.Sprintf("%f", v)
			default:
				val := reflect.ValueOf(value)
				if val.Kind() == reflect.Slice {
					// It's a slice/array
					formattedValue = "("

					for i := range val.Len() {
						elem := val.Index(i).Interface()
						if elem == nil {
							formattedValue += "NULL"
						} else {
							formattedValue += fmt.Sprintf("'%v'", elem)
						}
						if i < val.Len()-1 {
							formattedValue += ", "
						}
					}

					formattedValue += ")"
				} else {
					// Fallback
					formattedValue = fmt.Sprintf("'%v'", v)
				}
			}

			// lets create logic to handle case sensitive field and value

			if strings.Contains(fieldName, "__") {
				// handle fieldName that has double underscore this indicates that it is a relationship field
				foreignFieldSet := strings.Split(fieldName, "__")

				if len(foreignFieldSet) < 2 {
					continue
				}

				foreignFieldName := fmt.Sprintf("%v.%v", fieldName, foreignFieldSet[1])
				groupClauses = append(groupClauses, fmt.Sprintf("%s %s %s", foreignFieldName, operator, formattedValue))
			} else {
				groupClauses = append(groupClauses, fmt.Sprintf("%s %s %s", fmt.Sprintf("%v.%v", completeTableName, fieldName), operator, formattedValue))
			}

		}
		// Combine the group clauses with the group operator (AND/OR)
		if len(groupClauses) > 0 {
			operatorValue, ok := filterGroup.Operator.AsT2()

			// Fallback to "AND" if operator is not set or empty
			if !ok || operatorValue == "" {
				operatorValue = "AND"
			}

			clause := fmt.Sprintf("(%s)", strings.Join(groupClauses, fmt.Sprintf(" %s ", operatorValue)))
			filterClauses = append(filterClauses, clause)
		}
	}

	filterQuery := ""
	if len(filterClauses) > 0 {
		filterQuery = strings.Join(filterClauses, " AND ")
	}

	return filterQuery
}

// Helper function to build dynamic order by clauses
func buildOrderBy(request entity.CatalogQuery, columnsList []map[string]any) (string, map[string]string, []string) {
	var orderClauses []string
	joinQueryMap := make(map[string]string)
	joinQueryOrder := make([]string, 0)

	for _, order := range request.Orders {
		fieldName := order.FieldName
		// Handle chained fields (e.g., education_grade_id__grade_order)
		if strings.Contains(fieldName, "__") {
			parts := strings.Split(fieldName, "__")
			// Find the foreign table name from columnsList
			var foreignTableName string
			for _, col := range columnsList {
				if col[entity.FieldColumnCode].(string) == parts[0] {
					if foreignTable, ok := col[entity.FieldForeignTableName]; ok {
						foreignTableName = foreignTable.(string)
					}
					break
				}
			}

			if foreignTableName != "" {
				// Create a unique alias for this join
				joinAlias := fmt.Sprintf("order_%s_%s", parts[0], parts[1])
				foreignTableFullName := fmt.Sprintf("%v.%v", request.TenantCode, foreignTableName)
				mainTableName := fmt.Sprintf("%v.%v", request.TenantCode, request.ObjectCode)

				// Create join clause
				joinClause := fmt.Sprintf("LEFT JOIN %v as %v ON %v.%v = %v.%v",
					foreignTableFullName,
					joinAlias,
					joinAlias,
					"serial",
					mainTableName,
					parts[0])

				// Add to join maps if not exists
				if _, exists := joinQueryMap[joinAlias]; !exists {
					joinQueryMap[joinAlias] = joinClause
					joinQueryOrder = append(joinQueryOrder, joinAlias)
				}

				fieldName = fmt.Sprintf("%v.%v", joinAlias, parts[1])
			} else {
				fieldName = fmt.Sprintf("%v.%v.%v", request.TenantCode, request.ObjectCode, fieldName)
			}
		} else {
			fieldName = fmt.Sprintf("%v.%v.%v", request.TenantCode, request.ObjectCode, fieldName)
		}
		orderClauses = append(orderClauses, fmt.Sprintf("%s %s", fieldName, order.Direction))
	}
	return strings.Join(orderClauses, ", "), joinQueryMap, joinQueryOrder
}

func (r *repository) getSingleData(ctx context.Context, columnList []map[string]interface{}, columnsString, tableName string, request entity.CatalogQuery, joinQueryMap map[string]string, joinQueryOrder []string) string {
	// Start building the base query
	query := fmt.Sprintf(`
		SELECT %v
		FROM %v`, columnsString, tableName)

	// handle join table if any
	for _, joinKey := range joinQueryOrder {
		if !strings.Contains(query, joinQueryMap[joinKey]) {
			query = fmt.Sprintf("%s %s", query, joinQueryMap[joinKey])
		}
	}

	// checking if filters contains join table condition
	for _, filterGroup := range request.Filters {
		for fieldName, filter := range filterGroup.Filters {
			if strings.Contains(fieldName, "__") {
				queryResult, _ := r.HandleChainingJoinQuery(ctx, query, fieldName, tableName, request, filter)
				query = queryResult
			}
		}
	}

	// check if table has deleted_at column
	hasDeletedAt := false
	for _, column := range columnList {
		if column[entity.FieldColumnCode] == "deleted_at" {
			hasDeletedAt = true
			break
		}
	}

	if hasDeletedAt {
		query = query + fmt.Sprintf(" WHERE %v.deleted_at IS NULL", tableName)
	} else {
		query = query + " WHERE TRUE"
	}

	// Apply dynamic filters if they exist
	if len(request.Filters) > 0 {
		filterString := r.buildFilters(ctx, request)

		if len(filterString) > 0 {
			query = query + " AND " + filterString
		}
	}

	// // handle join table if any
	// for _, column := range columnList {
	// 	if column[entity.FieldForeignTableName] != nil && column[entity.FieldForeignColumnName] != nil {
	// 		foreignTableName := column[entity.FieldForeignTableName].(string)
	// 		foreignFieldName := column[entity.FieldForeignColumnName].(string)

	// 		joinClause := fmt.Sprintf("LEFT JOIN %v ON %v = %v.%v", foreignTableName, column[entity.FieldForeignColumnName], foreignTableName, foreignFieldName)

	// 		if !strings.Contains(query, joinClause) {
	// 			query = fmt.Sprintf("%s %s", query, joinClause)
	// 		}
	// 	}
	// }

	// // check if table has deleted_at column
	// hasDeletedAt := false
	// for _, column := range columnList {
	// 	if column[entity.FieldColumnCode] == "deleted_at" {
	// 		hasDeletedAt = true
	// 		break
	// 	}
	// }

	// if hasDeletedAt {
	// 	query = query + fmt.Sprintf(" WHERE %v.deleted_at IS NULL", tableName)
	// } else {
	// 	query = query + " WHERE TRUE"
	// }

	// apply serial to get single data
	identifierColumn := entity.DEFAULT_IDENTIFIER
	if !helper.IsUUID(request.Serial) {
		identifierColumn = "code"
	}

	query = query + fmt.Sprintf(" AND %v.%v = '%v'", tableName, identifierColumn, request.Serial)

	return query
}

// Main function to get data with pagination, filters, and orders
func (r *repository) getDataWithPagination(ctx context.Context, columnsString, tableName string, request entity.CatalogQuery, joinQueryMap map[string]string, joinQueryOrder []string, columnList []map[string]any) string {
	// Start building the base query
	query := fmt.Sprintf(`SELECT %v FROM %v`, columnsString, tableName)

	// Collect all join clauses
	var allJoins []string

	// Add existing joins
	for _, joinKey := range joinQueryOrder {
		if !strings.Contains(query, joinQueryMap[joinKey]) {
			allJoins = append(allJoins, joinQueryMap[joinKey])
		}
	}

	// checking if filters contains join table condition
	for _, filterGroup := range request.Filters {
		for fieldName, filter := range filterGroup.Filters {
			if strings.Contains(fieldName, "__") {
				queryResult, joinMap := r.HandleChainingJoinQuery(ctx, query, fieldName, tableName, request, filter)
				query = queryResult
				// Add any new joins from filter
				for _, joinClause := range joinMap {
					if !strings.Contains(query, joinClause) {
						allJoins = append(allJoins, joinClause)
					}
				}
			}
		}
	}

	// check if table has deleted_at column
	hasDeletedAt := false
	for _, column := range columnList {
		if column[entity.FieldColumnCode] == "deleted_at" {
			hasDeletedAt = true
			break
		}
	}

	// Build WHERE clause
	whereClause := "WHERE TRUE"
	if hasDeletedAt {
		whereClause = fmt.Sprintf("WHERE %v.deleted_at IS NULL", tableName)
	}

	// Apply dynamic filters if they exist
	if len(request.Filters) > 0 {
		filterString := r.buildFilters(ctx, request)
		if len(filterString) > 0 {
			whereClause = whereClause + " AND " + filterString
		}
	}

	// Apply dynamic order by if they exist
	if len(request.Orders) > 0 {
		orderString, orderJoinMap, orderJoinOrder := buildOrderBy(request, columnList)

		// Add any new joins from order by
		for _, joinKey := range orderJoinOrder {
			if !strings.Contains(query, orderJoinMap[joinKey]) {
				allJoins = append(allJoins, orderJoinMap[joinKey])
			}
		}

		// Combine all parts of the query
		query = fmt.Sprintf("%s %s %s ORDER BY %s",
			query,
			strings.Join(allJoins, " "),
			whereClause,
			orderString)
	} else {
		// If no order by, just combine the parts without ORDER BY
		query = fmt.Sprintf("%s %s %s",
			query,
			strings.Join(allJoins, " "),
			whereClause)
	}

	// Apply pagination (LIMIT and OFFSET)
	query = fmt.Sprintf("%s LIMIT %d OFFSET %d", query, request.PageSize, (request.Page-1)*request.PageSize)
	log.Print(query)

	return query
}

func (r *repository) getTotalCountQuery(ctx context.Context, tableName string, request entity.CatalogQuery, joinQueryMap map[string]string, joinQueryOrder []string, columnList []map[string]any) string {
	query := fmt.Sprintf(`SELECT COUNT(*) FROM %v`, tableName)

	// integrate join query if any
	for _, joinKey := range joinQueryOrder {
		if !strings.Contains(query, joinQueryMap[joinKey]) {
			query = fmt.Sprintf("%s %s", query, joinQueryMap[joinKey])
		}
	}

	// checking if filters contains join table condition
	for _, filterGroup := range request.Filters {
		for fieldName, filter := range filterGroup.Filters {
			if strings.Contains(fieldName, "__") {
				queryResult, _ := r.HandleChainingJoinQuery(ctx, query, fieldName, tableName, request, filter)
				query = queryResult

				// fmt.Print("joinQueryMap: ", joinQueryMap)
			}
		}
	}

	// check if table has deleted_at column
	hasDeletedAt := false
	for _, column := range columnList {
		if column[entity.FieldColumnCode] == "deleted_at" {
			hasDeletedAt = true
			break
		}
	}

	if hasDeletedAt {
		query = query + fmt.Sprintf(" WHERE %v.deleted_at IS NULL", tableName)
	} else {
		query = query + " WHERE TRUE"
	}

	// Apply dynamic filters if they exist
	if len(request.Filters) > 0 {
		filterString := r.buildFilters(ctx, request)

		if len(filterString) > 0 {
			query = query + " AND " + filterString
		}
	}

	log.Print(query)
	return query
}

func (r *repository) HandleChainingJoinQuery(ctx context.Context, query, fieldName, tableName string, request entity.CatalogQuery, filter entity.FilterItem) (updatedQuery string, joinQueryMap map[string]string) {
	// case example: user_serial__user_type_serial__name
	joinQueryMap = make(map[string]string)
	joinQuery := query

	foreignFieldSet := strings.Split(fieldName, "__")
	cleanTableName := strings.Split(tableName, ".")

	currentTableName := cleanTableName[0]
	if len(cleanTableName) > 1 {
		currentTableName = cleanTableName[1]
	}

	nextJoinAlias := ""

	for i, foreignField := range foreignFieldSet {
		if i < len(foreignFieldSet)-1 {
			foreignKeyInfo, _ := r.GetForeignKeyInfo(ctx, currentTableName, foreignField, request.TenantCode)
			foreignTableName := fmt.Sprintf("%v.%v", request.TenantCode, foreignKeyInfo.ForeignTable)

			// check if i is the last element
			joinAlias := fieldName
			if i < len(foreignFieldSet)-2 {
				joinAliasUpdated := fmt.Sprintf("%v__%v", currentTableName, foreignField)
				joinAlias = joinAliasUpdated
			}

			// clean join alias if it contains . convert into _
			joinAlias = strings.ReplaceAll(joinAlias, ".", "_")

			joinAliasField := joinAlias
			joinTableName := tableName
			if nextJoinAlias != "" {
				joinTableName = nextJoinAlias
			}

			foreignFieldName := fmt.Sprintf("%v.%v", joinAliasField, foreignKeyInfo.ForeignColumn)
			sourceFieldName := fmt.Sprintf("%v.%v", joinTableName, foreignField)

			joinClause := fmt.Sprintf("LEFT JOIN %v as %v ON %v = %v", foreignTableName, joinAlias, foreignFieldName, sourceFieldName)
			if !strings.Contains(joinQuery, joinClause) {
				joinQuery = fmt.Sprintf("%s %s", joinQuery, joinClause)
			}

			joinQueryMap[joinAlias] = joinClause

			currentTableName = foreignKeyInfo.ForeignTable
			nextJoinAlias = joinAlias
		}
	}

	return joinQuery, joinQueryMap
}

func (r *repository) HandleJoinQuery(ctx context.Context, query, fieldName, tableName string, request entity.CatalogQuery, filter entity.FilterItem) (updatedQuery string) {
	// if fieldName contains double underscore, then we need to join the table
	foreignFieldSet := strings.Split(fieldName, "__")

	// get foreign table name based on fieldName
	cleanTableName := strings.Split(tableName, ".")
	foreignKeyInfo, _ := r.GetForeignKeyInfo(ctx, cleanTableName[1], foreignFieldSet[0], request.TenantCode)

	foreignTableName := fmt.Sprintf("%v.%v", request.TenantCode, foreignKeyInfo.ForeignTable)
	foreignFieldName := fmt.Sprintf("%v.%v", foreignTableName, foreignKeyInfo.ForeignColumn)
	sourceFieldName := fmt.Sprintf("%v.%v", tableName, foreignFieldSet[0])

	joinClause := fmt.Sprintf("LEFT JOIN %v ON %v = %v", foreignTableName, foreignFieldName, sourceFieldName)

	if !strings.Contains(query, joinClause) {
		query = fmt.Sprintf("%s %s", query, joinClause)
	}

	// add filter condition to query
	operator := entity.OperatorQueryMap[filter.Operator]
	value := filter.Value

	// handler value of operator is part of entity.OperatorLIKEList, then we should add %
	if isOperatorInLIKEList(filter.Operator) {
		value = fmt.Sprintf("%%%v%%", value)
	}

	var formattedValue string

	switch v := value.(type) {
	case string:
		// Wrap strings in single quotes
		formattedValue = fmt.Sprintf("'%s'", v)
	case bool:
		// Booleans: PostgreSQL uses true/false literals
		formattedValue = fmt.Sprintf("%t", v)
	case int, int8, int16, int32, int64:
		formattedValue = fmt.Sprintf("%d", v)
	case float32, float64:
		formattedValue = fmt.Sprintf("%f", v)
	default:
		// Fallback to string with single quotes
		formattedValue = fmt.Sprintf("'%v'", v)
	}

	// Create filter conditions based on the field, operator, and value
	query = fmt.Sprintf("%s AND %s %s %s", query, fmt.Sprintf("%v.%v", foreignTableName, foreignFieldSet[1]), operator, formattedValue)

	return query
}
