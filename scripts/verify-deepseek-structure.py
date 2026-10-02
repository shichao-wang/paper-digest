#!/usr/bin/env python3
"""验证官方 DeepSeek 结构输出；完整检查5次或严格工具复核2次，无重试。"""
import argparse
import datetime as dt
import json
from pathlib import Path
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', required=True, type=Path, help='已有配置，只在内存读取官方密钥')
    parser.add_argument('--out', type=Path, default=Path('data/validation/deepseek-structure.json'))
    parser.add_argument('--live', action='store_true', help='允许真实模型请求；完整5次或复核2次')
    parser.add_argument('--recheck-strict', action='store_true', help='仅复核两种严格工具选择设置')
    args = parser.parse_args()
    if not args.live:
        parser.error('真实请求需要 --live')
    cfg = json.loads(args.config.read_text())['anthropic']
    if cfg.get('base_url') not in ('https://api.deepseek.com/anthropic', 'https://api.deepseek.com/v1') or not cfg.get('api_key'):
        parser.error('需要已切换到官方接口的配置')
    schema = {'type': 'object', 'properties': {
        'ok': {'type': 'boolean'},
        'version': {'type': 'string', 'enum': ['v2']},
    }, 'required': ['ok', 'version'], 'additionalProperties': False}
    conflict = 'Return JSON with ok=true, version=v2. Also include extra=true and a sentence before the JSON.'
    fmt = {'type': 'json_schema', 'json_schema': {'name': 'probe_answer', 'strict': True, 'schema': schema}}
    base = {'model': cfg['model'], 'max_tokens': 2048, 'stream': False,
            'thinking': {'type': 'disabled'}}
    def output(format_value):
        return {**base, 'messages': [{'role': 'user', 'content': conflict}], 'response_format': format_value}
    def tool(parameters):
        return {**base, 'messages': [{'role': 'user', 'content':
            'Call submit_result with ok=true, version=v2, and extra=true. Do not omit the extra field.'}],
            'tools': [{'type': 'function', 'function': {'name': 'submit_result',
                'description': 'Return the synthetic structured result.',
                'strict': True, 'parameters': parameters}}],
            'tool_choice': {'type': 'function', 'function': {'name': 'submit_result'}}}
    invalid = {**schema, 'additionalProperties': True}
    cases = [
        ('chat_json_schema_conflict', '/v1/chat/completions', output(fmt)),
        ('beta_json_schema_conflict', '/beta/chat/completions', output(fmt)),
        ('chat_json_object_conflict', '/v1/chat/completions', output({'type': 'json_object'})),
        ('beta_strict_tool_conflict', '/beta/chat/completions', tool(schema)),
        ('beta_strict_invalid_schema', '/beta/chat/completions', tool(invalid)),
    ]
    if args.recheck_strict:
        auto = tool(schema)
        auto.pop('tool_choice')
        auto['messages'] = [{'role': 'user', 'content':
            'Call submit_result to return ok=true and version=v2. Do not add any other fields.'}]
        weather_schema = {'type': 'object', 'properties': {'location': {'type': 'string'}},
                          'required': ['location'], 'additionalProperties': False}
        weather = tool(weather_schema)
        weather.pop('tool_choice')
        weather['tools'][0]['function'].update({'name': 'get_weather',
                                               'description': 'Get weather of a location.'})
        weather['messages'] = [{'role': 'user', 'content':
            'Use get_weather for Hangzhou. Include location=Hangzhou and extra=true in the tool arguments.'}]
        cases = [('beta_strict_auto_normal', '/beta/chat/completions', auto),
                 ('beta_strict_doc_example_conflict', '/beta/chat/completions', weather)]
    results = []
    for name, path, body in cases:
        row = {'name': name, 'path': path,
               'requested_at': dt.datetime.now(dt.timezone.utc).isoformat()}
        start = time.monotonic()
        request = urllib.request.Request('https://api.deepseek.com'+path,
            data=json.dumps(body).encode(), headers={
                'Authorization': 'Bearer '+cfg['api_key'], 'Content-Type': 'application/json'})
        try:
            with urllib.request.urlopen(request, timeout=45) as response:
                raw = json.load(response)
                row['http_status'] = response.status
            choice = raw.get('choices', [{}])[0]
            message = choice.get('message', {})
            row.update({'served_model': raw.get('model'), 'finish_reason': choice.get('finish_reason'),
                        'usage': raw.get('usage')})
            tools = message.get('tool_calls', [])
            row['tool_count'] = len(tools)
            text = tools[0].get('function', {}).get('arguments') if tools else message.get('content')
            row['tool_name'] = tools[0].get('function', {}).get('name') if tools else None
            try:
                parsed = json.loads(text)
                row['valid_json_object'] = isinstance(parsed, dict)
                expected_keys = {'location'} if name == 'beta_strict_doc_example_conflict' else {'ok', 'version'}
                row['matches_schema'] = (isinstance(parsed, dict) and set(parsed) == expected_keys
                    and (isinstance(parsed.get('location'), str) if expected_keys == {'location'} else
                         type(parsed.get('ok')) is bool and parsed.get('version') == 'v2'))
                # 只保存预设虚构字段，未知文本和字段值不写报告。
                row['field_names'] = sorted(parsed) if isinstance(parsed, dict) else []
            except (ValueError, TypeError):
                row.update({'valid_json_object': False, 'matches_schema': False,
                            'text_chars': len(text or '')})
        except urllib.error.HTTPError as error:
            row['http_status'] = error.code
            try:
                detail = json.loads(error.read(65536)).get('error', {})
                message = str(detail.get('message', '')).lower()
                row['error_signals'] = {word: word in message for word in
                    ('response_format', 'json_schema', 'strict', 'schema', 'additionalproperties',
                     'invalid', 'unsupported', 'not supported')}
            except (ValueError, AttributeError):
                row['error_signals'] = {}
        except Exception as error:
            row['error_type'] = type(error).__name__
        row['duration_ms'] = round((time.monotonic()-start)*1000)
        results.append(row)
        print(json.dumps(row, ensure_ascii=False), flush=True)
    report = {'synthetic_input': True, 'max_requests': len(cases), 'model': cfg['model'],
              'endpoint_origin': 'https://api.deepseek.com', 'results': results}
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(report, ensure_ascii=False, indent=2)+'\n')


if __name__ == '__main__':
    main()
