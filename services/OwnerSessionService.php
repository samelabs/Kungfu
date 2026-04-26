<?php

require_once dirname(__DIR__) . '/core/OwnerSession.php';
require_once dirname(__DIR__) . '/presenters/IdentityPresenter.php';
require_once dirname(__DIR__) . '/exceptions/AppException.php';

class OwnerSessionService
{
    public static function current(): array
    {
        $bot = OwnerSession::current();
        if (!$bot) {
            throw new AppException(401, 'OWNER_LOGIN_REQUIRED', 'Owner login required');
        }

        return IdentityPresenter::ownerSession($bot);
    }

    public static function login(string $name, string $password): array
    {
        return IdentityPresenter::ownerSession(OwnerSession::login($name, $password));
    }

    public static function logout(): array
    {
        OwnerSession::logout();
        return [];
    }
}
