ALTER TABLE connections DROP CONSTRAINT connections_auth_method_check;
ALTER TABLE connections
    ADD COLUMN auth_name text,
    ADD CONSTRAINT connections_auth_method_check
        CHECK (auth_method IN ('none', 'bearer', 'header', 'oauth2')),
    ADD CONSTRAINT connections_auth_name_check
        CHECK ((auth_method = 'header' AND auth_name IS NOT NULL AND length(auth_name) BETWEEN 1 AND 120)
            OR (auth_method <> 'header' AND auth_name IS NULL));

ALTER TABLE http_tool_specs RENAME COLUMN path_template TO url_template;
ALTER TABLE http_tool_specs DROP CONSTRAINT http_tool_specs_path_template_check;
ALTER TABLE http_tool_specs DROP CONSTRAINT http_tool_specs_timeout_ms_check;
ALTER TABLE http_tool_specs DROP CONSTRAINT http_tool_specs_max_response_bytes_check;
ALTER TABLE http_tool_specs
    ADD CONSTRAINT http_tool_specs_timeout_ms_check CHECK (timeout_ms BETWEEN 100 AND 3600000),
    ADD CONSTRAINT http_tool_specs_max_response_bytes_check CHECK (max_response_bytes BETWEEN 1024 AND 33554432);

ALTER TABLE command_tool_specs DROP CONSTRAINT command_tool_specs_timeout_ms_check;
ALTER TABLE command_tool_specs DROP CONSTRAINT command_tool_specs_max_output_bytes_check;
ALTER TABLE command_tool_specs
    ADD COLUMN working_directory text,
    ADD CONSTRAINT command_tool_specs_timeout_ms_check CHECK (timeout_ms BETWEEN 100 AND 3600000),
    ADD CONSTRAINT command_tool_specs_max_output_bytes_check CHECK (max_output_bytes BETWEEN 1024 AND 33554432);
