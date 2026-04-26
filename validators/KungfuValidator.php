<?php

require_once dirname(__DIR__) . '/core/Security.php';
require_once dirname(__DIR__) . '/core/KungfuUtils.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';

class KungfuValidator
{
    public static function validatePayload(array $input, array $config): array
    {
        foreach (['title', 'tags', 'content'] as $field) {
            if (empty($input[$field])) {
                throw new AppException(400, 'MISSING_FIELD', "Missing required field: {$field}");
            }
        }

        $code = isset($input['code']) ? trim((string)$input['code']) : '';
        $title = trim((string)$input['title']);
        $tags = $input['tags'];
        $description = isset($input['description']) ? trim((string)$input['description']) : '';
        $content = (string)$input['content'];

        if ($code !== '') {
            $code = KungfuUtils::validateCode($code);
        }

        if ($title === '') {
            throw new AppException(400, 'MISSING_FIELD', 'Missing required field: title');
        }

        if (mb_strlen($title) > (int)$config['max_title_length']) {
            throw new AppException(400, 'TITLE_TOO_LONG', 'Title maximum 128 characters');
        }

        if (!is_array($tags)) {
            throw new AppException(400, 'INVALID_TAGS', 'tags must be an array');
        }

        if (count($tags) < 1) {
            throw new AppException(400, 'INVALID_TAGS', 'At least one tag is required');
        }

        if (count($tags) > (int)$config['max_tags']) {
            throw new AppException(400, 'TOO_MANY_TAGS', 'Maximum 10 tags');
        }

        foreach ($tags as $index => $tag) {
            if (!is_string($tag)) {
                throw new AppException(400, 'INVALID_TAGS', 'Each tag must be a string');
            }
            $tags[$index] = trim($tag);
            if ($tags[$index] === '') {
                throw new AppException(400, 'INVALID_TAGS', 'Tags cannot be empty');
            }
            if (mb_strlen($tags[$index]) > (int)$config['max_tag_length']) {
                throw new AppException(400, 'TAG_TOO_LONG', 'Each tag maximum 32 characters');
            }
        }

        if (mb_strlen($description) > (int)$config['max_description_length']) {
            throw new AppException(400, 'DESCRIPTION_TOO_LONG', 'Description maximum 500 characters');
        }

        if (strlen($content) > (int)$config['max_content_size']) {
            throw new AppException(400, 'CONTENT_TOO_LARGE', 'Content exceeds 100KB limit');
        }

        if (mb_strlen(trim($content)) < 50) {
            throw new AppException(400, 'CONTENT_TOO_SHORT', 'Content too short (minimum 50 characters)');
        }

        Security::rejectApiKeyInContent([
            'code' => $code,
            'title' => $title,
            'tags' => $tags,
            'description' => $description,
            'content' => $content
        ], 'kungfu payload');

        return [
            'code' => $code,
            'title' => $title,
            'tags' => array_values($tags),
            'description' => $description,
            'content' => $content,
            'checksum' => hash('sha256', $content)
        ];
    }
}
