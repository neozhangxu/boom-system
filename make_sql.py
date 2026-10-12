import xlrd

wb = xlrd.open_workbook('094.xls')
sheet = wb.sheet_by_index(0)

sql_lines = []
sql_lines.append("SET NAMES utf8mb4;")
sql_lines.append("START TRANSACTION;\n")
sql_lines.append("DROP TABLE IF EXISTS bom_item_color_detail;")
sql_lines.append("DROP TABLE IF EXISTS bom_item;")
sql_lines.append("DROP TABLE IF EXISTS bom_parts;")
sql_lines.append("DROP TABLE IF EXISTS bom_colorway;")
sql_lines.append("DROP TABLE IF EXISTS bom_header;\n")

sql_lines.append('''CREATE TABLE bom_header (
    id INT AUTO_INCREMENT PRIMARY KEY,
    sample_order_no VARCHAR(100),
    sample_type VARCHAR(100),
    create_date VARCHAR(50),
    print_date VARCHAR(50),
    season VARCHAR(50),
    request_finish_date VARCHAR(50),
    gender VARCHAR(50),
    style_no VARCHAR(100),
    style_name VARCHAR(200),
    last_no VARCHAR(100),
    bottom_no VARCHAR(100),
    shoe_filler_no VARCHAR(100),
    shoetype VARCHAR(200),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;''')

sql_lines.append('''CREATE TABLE bom_colorway (
    id INT AUTO_INCREMENT PRIMARY KEY,
    header_id INT,
    color_code VARCHAR(100),
    FOREIGN KEY (header_id) REFERENCES bom_header(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;''')

sql_lines.append('''CREATE TABLE bom_parts (
    id INT AUTO_INCREMENT PRIMARY KEY,
    header_id INT,
    part_seq INT,
    part_name VARCHAR(200),
    FOREIGN KEY (header_id) REFERENCES bom_header(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;''')

sql_lines.append('''CREATE TABLE bom_item (
    id INT AUTO_INCREMENT PRIMARY KEY,
    header_id INT,
    part_id INT,
    item_seq INT,
    material_desc TEXT,
    unit VARCHAR(50),
    remark VARCHAR(255),
    supplier VARCHAR(200),
    FOREIGN KEY (header_id) REFERENCES bom_header(id) ON DELETE CASCADE,
    FOREIGN KEY (part_id) REFERENCES bom_parts(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;''')

sql_lines.append('''CREATE TABLE bom_item_color_detail (
    id INT AUTO_INCREMENT PRIMARY KEY,
    bom_item_id INT,
    colorway_id INT,
    material_color_spec TEXT,
    process_desc TEXT,
    FOREIGN KEY (bom_item_id) REFERENCES bom_item(id) ON DELETE CASCADE,
    FOREIGN KEY (colorway_id) REFERENCES bom_colorway(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n''')

def esc(val):
    if val is None or val == '':
        return "NULL"
    s = str(val).replace("'", "''")
    return f"'{s}'"

def get_cell(r, c):
    if r < sheet.nrows and c < sheet.ncols:
        v = sheet.cell_value(r, c)
        if isinstance(v, float) and v.is_integer():
            return str(int(v))
        return str(v).strip()
    return ''

sample_order_no = get_cell(3, 1)
sample_type = get_cell(3, 3)
create_date_raw = get_cell(3, 5)
create_date = create_date_raw.split('print')[0].replace('Create Date:', '').strip()
print_date = create_date_raw.split('print date:')[1].strip() if 'print date:' in create_date_raw else ''
season = get_cell(4, 2)
request_finish_date = get_cell(4, 5)
gender = get_cell(5, 2)
style_no = get_cell(6, 1)
style_name = get_cell(6, 3)
last_no = get_cell(7, 1)
bottom_no = get_cell(7, 3)
shoe_filler_no = get_cell(7, 5)
shoetype = get_cell(8, 1)

sql_lines.append(f"INSERT INTO bom_header (id, sample_order_no, sample_type, create_date, print_date, season, request_finish_date, gender, style_no, style_name, last_no, bottom_no, shoe_filler_no, shoetype) VALUES (1, {esc(sample_order_no)}, {esc(sample_type)}, {esc(create_date)}, {esc(print_date)}, {esc(season)}, {esc(request_finish_date)}, {esc(gender)}, {esc(style_no)}, {esc(style_name)}, {esc(last_no)}, {esc(bottom_no)}, {esc(shoe_filler_no)}, {esc(shoetype)});\n")

color_cols = [7, 9, 11, 13, 15, 17, 19, 21]
color_id_map = {}
for idx, c in enumerate(color_cols, start=1):
    code = get_cell(10, c)
    color_id_map[c] = idx
    sql_lines.append(f"INSERT INTO bom_colorway (id, header_id, color_code) VALUES ({idx}, 1, {esc(code)});\n")

current_part_id = 0
current_item_id = 0
last_part_seq = 0
last_part_name = ""

r = 12
while r < sheet.nrows:
    col0 = get_cell(r, 0)
    col2 = get_cell(r, 2)
    
    if col0 != '' and not col0.isdigit():
        r += 1
        continue
        
    if col0 != '' and col0.isdigit():
        part_seq = int(col0)
        part_name_val = get_cell(r, 1)
        if part_name_val != '':
            last_part_name = part_name_val
            
        material_desc = col2
        unit = get_cell(r, 5)
        remark = get_cell(r, 6)
        supplier = get_cell(r, 23)
        
        current_part_id += 1
        sql_lines.append(f"INSERT INTO bom_parts (id, header_id, part_seq, part_name) VALUES ({current_part_id}, 1, {part_seq}, {esc(last_part_name)});")
        
        current_item_id += 1
        sql_lines.append(f"INSERT INTO bom_item (id, header_id, part_id, item_seq, material_desc, unit, remark, supplier) VALUES ({current_item_id}, 1, {current_part_id}, {part_seq}, {esc(material_desc)}, {esc(unit)}, {esc(remark)}, {esc(supplier)});")
        
        for col_idx, colorway_id in color_id_map.items():
            spec = get_cell(r, col_idx)
            proc = get_cell(r, col_idx + 1)
            if spec != '' or proc != '':
                sql_lines.append(f"INSERT INTO bom_item_color_detail (bom_item_id, colorway_id, material_color_spec, process_desc) VALUES ({current_item_id}, {colorway_id}, {esc(spec)}, {esc(proc)});")
                
        r_sub = r + 1
        while r_sub < sheet.nrows and get_cell(r_sub, 0) == '' and get_cell(r_sub, 2) != '':
            sub_row_desc = get_cell(r_sub, 2)
            sub_unit = get_cell(r_sub, 5) if get_cell(r_sub, 5) != '' else unit
            
            current_item_id += 1
            sql_lines.append(f"INSERT INTO bom_item (id, header_id, part_id, item_seq, material_desc, unit, remark, supplier) VALUES ({current_item_id}, 1, {current_part_id}, {part_seq}, {esc(sub_row_desc)}, {esc(sub_unit)}, '', '');")
            
            for col_idx, colorway_id in color_id_map.items():
                spec = get_cell(r_sub, col_idx)
                proc = get_cell(r_sub, col_idx + 1)
                if spec != '' or proc != '':
                    sql_lines.append(f"INSERT INTO bom_item_color_detail (bom_item_id, colorway_id, material_color_spec, process_desc) VALUES ({current_item_id}, {colorway_id}, {esc(spec)}, {esc(proc)});")
            r_sub += 1
        r = r_sub
    else:
        r += 1

sql_lines.append("\nCOMMIT;")

with open('exact_full_bom.sql', 'w', encoding='utf-8') as f:
    f.write('\n'.join(sql_lines))

print(f"SUCCESS! Generated exact_full_bom.sql. Total items: {current_item_id}")
